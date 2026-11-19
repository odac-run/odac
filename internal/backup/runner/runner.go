// Package runner starts odac-backup for one request and drives the
// protocol with it. Inside a container (production) every run gets a
// helper container from ODAC's own image, with the run's directories
// mounted and nothing else, and a backup's dump comes from a second
// container made from the database app's image; outside one (development)
// odac-backup runs as a child process and sees the server's paths as they
// are. Either way the server holds the only connection between the two,
// so the helper never needs the Docker socket.
//
// A helper outlives the server that started it when an update replaces
// that server: Release hands the running helpers over, and the next
// server finds them with Adopt and reads their results from their logs.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/docker"
)

// ErrReleased is a run handed over by Release: its helper keeps running
// and the server that replaces this one reports its result.
var ErrReleased = errors.New("runner: run handed over to the next server")

// reapTimeout bounds the wait for a helper's exit after its last event.
const reapTimeout = time.Minute

// Labels a run's containers carry, besides the caller's.
const (
	LabelOp  = docker.LabelBackupPrefix + "op"
	LabelRun = docker.LabelBackupPrefix + "run"
)

// Process is one running odac-backup.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() string
	Wait(ctx context.Context) (docker.ExitStatus, error)
	// Stop asks it to end, which a run sees as canceled.
	Stop()
	// Remove ends it if it still runs and frees what it held.
	Remove()
	// Detach drops the connection and leaves it running.
	Detach()
}

// Docker is the slice of the container runtime the runner uses;
// FromClient adapts *docker.Client.
type Docker interface {
	SelfImage() (string, error)
	StartHelper(ctx context.Context, spec docker.HelperSpec) (Process, error)
	StartDump(ctx context.Context, spec docker.DumpSpec) (io.ReadCloser, error)
	BackupContainers() ([]docker.BackupContainer, error)
	FollowHelper(ctx context.Context, id string) (io.ReadCloser, error)
	WaitBackupContainer(ctx context.Context, id string) (docker.ExitStatus, error)
	RemoveBackupContainer(b docker.BackupContainer)
	SweepBackupVolumes()
}

// FromClient adapts the Docker client to Docker.
func FromClient(c *docker.Client) Docker { return clientAdapter{c} }

type clientAdapter struct{ *docker.Client }

func (a clientAdapter) StartHelper(ctx context.Context, spec docker.HelperSpec) (Process, error) {
	return a.Client.StartHelper(ctx, spec)
}

func (a clientAdapter) StartDump(ctx context.Context, spec docker.DumpSpec) (io.ReadCloser, error) {
	return a.Client.StartDump(ctx, spec)
}

// Config sets how runs are started.
type Config struct {
	// Docker starts helpers and dumps. It is required in container mode
	// and for any dump.
	Docker Docker
	// Local runs odac-backup as a child process instead of a container:
	// development, where the server is not in a container.
	Local bool
	// Binary is odac-backup's path, the same inside ODAC's image as in this
	// process's file system; empty means next to this executable.
	Binary string
	// Image is the helper's image; empty means the image of the container
	// this server runs in.
	Image string
	// Limits bound each helper, DumpLimits each dump container.
	Limits     docker.Limits
	DumpLimits docker.Limits
	// DumpRate caps how fast a dump is read, in bytes per second. The
	// database slows down with its reader, which keeps a dump from
	// competing with the app for the disk.
	DumpRate int64
}

// Runner starts runs. It is safe for concurrent use; deciding which runs
// may overlap (one heavy backup per server, never a backup and
// maintenance in one repository) is the caller's.
type Runner struct {
	cfg Config

	mu       sync.Mutex
	released bool
	release  chan struct{}
}

// New checks cfg and fills its defaults.
func New(cfg Config) (*Runner, error) {
	if cfg.Binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("runner: locate odac-backup: %w", err)
		}
		cfg.Binary = filepath.Join(filepath.Dir(exe), "odac-backup")
	}
	if !filepath.IsAbs(cfg.Binary) {
		return nil, errors.New("runner: odac-backup path must be absolute")
	}
	if !cfg.Local && cfg.Docker == nil {
		return nil, errors.New("runner: a container runner needs Docker")
	}
	if cfg.DumpRate <= 0 {
		return nil, errors.New("runner: a dump rate is required")
	}
	return &Runner{cfg: cfg, release: make(chan struct{})}, nil
}

// InContainer reports whether this process runs in a container, which is
// what decides between helper containers and child processes.
func InContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// Target says where a filesystem repository lives when it is not a path
// this server can see: the request's Filesystem path is then taken
// inside it, "/" being its root.
type Target struct {
	// Root is a directory on the host, as the operator named it.
	Root string
	// Share is an SMB or NFS export instead; its Target is ignored.
	Share *docker.Share
}

// Invocation is one request and what it needs besides the request.
type Invocation struct {
	// Request names paths as this server sees them (or inside Target);
	// the runner rewrites them to the helper's view.
	Request *proto.Request
	// Handler receives events. Its Stream must be nil: Dump supplies it.
	Handler proto.Handler
	// Dump produces the stream of a backup whose first item is one.
	Dump   *docker.DumpSpec
	Target *Target
	// Labels go on the run's containers, under docker.LabelBackupPrefix.
	Labels map[string]string
}

// Run runs one request and reaps its helper. Canceling ctx stops the run
// on both sides: a dump in flight is killed at once and the helper is
// stopped, which ends it as canceled with its repository closed.
func (r *Runner) Run(ctx context.Context, inv Invocation) (*proto.Result, error) {
	if inv.Request == nil {
		return nil, errors.New("runner: no request")
	}
	if inv.Handler.Stream != nil {
		return nil, errors.New("runner: the stream comes from Dump")
	}
	if (inv.Dump != nil) != inv.Request.HasStream() {
		return nil, errors.New("runner: a stream item and a dump go together")
	}
	if inv.Dump != nil && r.cfg.Docker == nil {
		return nil, errors.New("runner: a dump needs Docker")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopRelease := r.onRelease(cancel)
	defer stopRelease()

	req, p, err := r.start(runCtx, inv)
	if err != nil {
		return nil, err
	}

	h := inv.Handler
	var dump *dumpStream
	if inv.Dump != nil {
		spec := *inv.Dump
		spec.Limits = r.cfg.DumpLimits
		spec.Labels = r.labels(inv)
		dump = newDumpStream(r.cfg.DumpRate, func() (io.ReadCloser, error) {
			return r.cfg.Docker.StartDump(runCtx, spec)
		})
		defer dump.Close()
		h.Stream = dump
	}

	res, callErr := proto.Call(runCtx, p.Stdin(), p.Stdout(), req, h)
	if dump != nil {
		dump.Close()
	}

	if callErr != nil && r.isReleased() && ctx.Err() == nil {
		p.Detach()
		return nil, ErrReleased
	}
	defer p.Remove()
	if ctx.Err() != nil {
		p.Stop()
		return nil, ctx.Err()
	}
	// A helper that reported its own failure is ending; any other failure
	// (it died, or the server refused what it sent) may leave it running.
	var pe *proto.Error
	if callErr != nil && !errors.As(callErr, &pe) {
		p.Stop()
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), reapTimeout)
	defer wcancel()
	st, werr := p.Wait(wctx)
	if callErr != nil {
		return nil, explain(callErr, st, werr, p.Stderr())
	}
	return res, nil
}

// explain adds how the helper ended to a run that ended without a result,
// so an out-of-memory kill reads as one.
func explain(err error, st docker.ExitStatus, werr error, stderr string) error {
	if !errors.Is(err, proto.ErrNoResult) || werr != nil {
		return err
	}
	if stderr != "" {
		return fmt.Errorf("%w: odac-backup %s: %s", err, st, lastLine(stderr))
	}
	return fmt.Errorf("%w: odac-backup %s", err, st)
}

func lastLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return s[i+1:]
		}
	}
	return s
}

// Release hands every run in flight, and every later one, to the server
// that replaces this one: their helpers keep running and Run returns
// ErrReleased. A dump in flight is still killed, since its stream ends
// with this server; the helper then stores nothing for it.
func (r *Runner) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.released {
		r.released = true
		close(r.release)
	}
}

func (r *Runner) isReleased() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.released
}

func (r *Runner) onRelease(f func()) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-r.release:
			f()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (r *Runner) labels(inv Invocation) map[string]string {
	out := make(map[string]string, len(inv.Labels)+2)
	for k, v := range inv.Labels {
		out[k] = v
	}
	out[LabelOp] = inv.Request.Op
	if b := inv.Request.Backup; b != nil {
		out[LabelRun] = b.Run
	}
	return out
}

// start launches the helper for inv and returns the request as the helper
// must see it.
func (r *Runner) start(ctx context.Context, inv Invocation) (*proto.Request, Process, error) {
	if r.isReleased() {
		return nil, nil, ErrReleased
	}
	if r.cfg.Local {
		req, err := localRequest(inv)
		if err != nil {
			return nil, nil, err
		}
		p, err := startLocal(r.cfg.Binary)
		return req, p, err
	}
	image := r.cfg.Image
	if image == "" {
		var err error
		if image, err = r.cfg.Docker.SelfImage(); err != nil {
			return nil, nil, fmt.Errorf("runner: %w", err)
		}
	}
	req, spec, err := helperSpec(inv)
	if err != nil {
		return nil, nil, err
	}
	spec.Image = image
	spec.Entrypoint = []string{r.cfg.Binary, "run"}
	spec.Limits = r.cfg.Limits
	spec.Labels = r.labels(inv)
	p, err := r.cfg.Docker.StartHelper(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	return req, p, nil
}
