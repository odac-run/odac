package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
)

// Labels on every backup container and share volume. LabelBackupRole is
// what finds them after an update; the rest are the caller's, under the
// LabelBackupPrefix namespace.
const (
	LabelBackupPrefix = "odac.backup."
	LabelBackupRole   = LabelBackupPrefix + "role"
	// LabelBackupVolume names the share volume a helper mounts, so the
	// volume goes with the container.
	LabelBackupVolume = LabelBackupPrefix + "volume"
	// LabelBackupFollow marks a helper whose output stays in its log, so a
	// server that did not start it can read the run's events from the top.
	LabelBackupFollow = LabelBackupPrefix + "follow"
)

// Backup container roles.
const (
	RoleHelper = "helper"
	RoleDump   = "dump"
	RoleShare  = "share"
)

// helperCaps is every capability a helper may be given. Reading app data
// owned by other users needs DAC_READ_SEARCH; a target owned by another
// user needs DAC_OVERRIDE; restoring ownership and modes needs the rest.
// Nothing that reaches past the file system is here.
var helperCaps = []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID"}

// stopGrace is how long a stopped helper has to close its repository
// before the daemon kills it.
const stopGrace = 30 * time.Second

// helperLogSize bounds a followable helper's log. Its output is progress
// every two seconds plus one line per stored snapshot, so this holds any
// run's events many times over.
const helperLogSize = "16m"

// tmpfsSize bounds the scratch space of a backup container, whose root
// file system is read-only.
const tmpfsSize = "rw,noexec,nosuid,nodev,size=64m"

// stderrTail is how much of a container's stderr is kept for its error.
const stderrTail = 4 << 10

// Limits are a backup container's resources. Memory and PidsLimit are
// required: a helper or dump without them could take the host down with
// it, which is the failure the separate container exists to contain.
type Limits struct {
	// NanoCPUs caps CPU time (1e9 is one core); zero leaves it uncapped.
	NanoCPUs int64
	// CPUShares is the weight under contention (the daemon's default is
	// 1024); zero leaves the default.
	CPUShares int64
	// Memory is a hard limit with no swap beyond it, so a run that
	// outgrows it is killed instead of pushing the host into swap.
	Memory    int64
	PidsLimit int64
	// BlkioWeight is the I/O weight, 10 to 1000 (default 500); zero leaves
	// the default. It applies only where the host's I/O scheduler honors
	// weights.
	BlkioWeight uint16
}

func (l Limits) validate() error {
	if l.Memory <= 0 || l.PidsLimit <= 0 {
		return errors.New("backup container: memory and pid limits are required")
	}
	if l.NanoCPUs < 0 || l.CPUShares < 0 {
		return errors.New("backup container: negative CPU limit")
	}
	if l.BlkioWeight != 0 && (l.BlkioWeight < 10 || l.BlkioWeight > 1000) {
		return errors.New("backup container: I/O weight must be 10 to 1000")
	}
	return nil
}

func (l Limits) resources() container.Resources {
	pids := l.PidsLimit
	return container.Resources{
		NanoCPUs:    l.NanoCPUs,
		CPUShares:   l.CPUShares,
		Memory:      l.Memory,
		MemorySwap:  l.Memory,
		PidsLimit:   &pids,
		BlkioWeight: l.BlkioWeight,
	}
}

// HelperMount is one directory a helper sees.
type HelperMount struct {
	// Source is a path as this process sees it, translated to the host's
	// view like every other bind; HostSource marks one that already is a
	// host path, which is what a target directory chosen by the operator
	// is.
	Source     string
	HostSource bool
	Target     string
	ReadOnly   bool
}

// Share is an SMB or NFS export the daemon mounts on the host for a
// helper, through the built-in local volume driver, so ODAC's container
// needs no mount privilege. The volume lives as long as the helper: its
// options carry the SMB password, which `docker volume inspect` shows.
type Share struct {
	// Type is "cifs" (SMB) or "nfs".
	Type string
	// Addr is the server's host name or address.
	Addr string
	// Path is the share name for SMB ("backups") or the export path for
	// NFS ("/srv/backups").
	Path string
	// Username, Password and Domain authenticate SMB.
	Username string
	Password string
	Domain   string
	// Version is the protocol version ("3.0" for SMB, "4.1" for NFS);
	// empty lets the kernel negotiate.
	Version string
	Target  string
}

var (
	shareAddrPattern = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]+$`)
	versionPattern   = regexp.MustCompile(`^[0-9][0-9.]*$`)
)

// Validate refuses a share the daemon would be asked to mount with options
// it did not mean, so a target can be refused when it is saved rather than
// at its first run.
func (s Share) Validate() error {
	_, err := s.driverOpts()
	return err
}

// driverOpts builds the local driver's options. Mount options are one
// comma-separated string the kernel parses, so a comma or an equals sign
// inside a value would add options of the caller's choosing (uid=, or a
// second addr=); such values are refused rather than escaped.
func (s Share) driverOpts() (map[string]string, error) {
	if !shareAddrPattern.MatchString(s.Addr) {
		return nil, fmt.Errorf("share: invalid server address %q", s.Addr)
	}
	if s.Version != "" && !versionPattern.MatchString(s.Version) {
		return nil, fmt.Errorf("share: invalid protocol version %q", s.Version)
	}
	if s.Path == "" || strings.ContainsAny(s.Path, ",\x00\n") {
		return nil, errors.New("share: invalid path")
	}
	for name, v := range map[string]string{"username": s.Username, "password": s.Password, "domain": s.Domain} {
		if strings.ContainsAny(v, ",\x00\n") || (name != "password" && strings.Contains(v, "=")) {
			return nil, fmt.Errorf("share: the %s cannot contain a comma", name)
		}
	}
	addr := strings.Trim(s.Addr, "[]")
	o := []string{"addr=" + addr}
	switch s.Type {
	case "cifs":
		if strings.HasPrefix(s.Path, "/") {
			return nil, errors.New("share: an SMB share is named without a leading slash")
		}
		if s.Username != "" {
			o = append(o, "username="+s.Username, "password="+s.Password)
		} else {
			o = append(o, "guest")
		}
		if s.Domain != "" {
			o = append(o, "domain="+s.Domain)
		}
		if s.Version != "" {
			o = append(o, "vers="+s.Version)
		}
		host := s.Addr
		if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
			host = "[" + addr + "]"
		}
		return map[string]string{"type": "cifs", "device": "//" + host + "/" + s.Path, "o": strings.Join(o, ",")}, nil
	case "nfs":
		if !path.IsAbs(s.Path) || s.Username != "" || s.Password != "" {
			return nil, errors.New("share: an NFS export is an absolute path and takes no credentials")
		}
		if s.Version != "" {
			o = append(o, "nfsvers="+s.Version)
		}
		return map[string]string{"type": "nfs", "device": ":" + path.Clean(s.Path), "o": strings.Join(o, ",")}, nil
	}
	return nil, fmt.Errorf("share: unknown type %q", s.Type)
}

// HelperSpec is one odac-backup run in its own container.
type HelperSpec struct {
	// Image is ODAC's own image id (SelfImage); Entrypoint the odac-backup
	// binary inside it.
	Image      string
	Entrypoint []string
	// Labels are the caller's, all under LabelBackupPrefix.
	Labels map[string]string
	Mounts []HelperMount
	Share  *Share
	// Network gives the helper egress on BackupNetwork, for a remote
	// repository. Without it the helper has no network at all.
	Network bool
	// Caps is the subset of helperCaps this run needs.
	Caps   []string
	Limits Limits
	// Follow keeps the helper's output in its log, so a server that did
	// not start it can replay the run (FollowHelper). Leave it off for a
	// run whose output carries data, such as a stream restore, which
	// would otherwise land in a log file on the host.
	Follow bool
}

// DumpSpec is a database dump in its own container: the database app's
// image, so the dump tool matches the server's version, inside the app
// container's network namespace, so it reaches the database on loopback
// whatever networks the app is on.
type DumpSpec struct {
	Image string
	// App is the container whose network namespace the dump joins.
	App string
	// Cmd is the dump command; its first element replaces the image's
	// entrypoint, so the image never starts a database of its own.
	Cmd []string
	// Env and Cmd are shown by `docker inspect`, and Cmd also by `ps` on
	// the host, so neither may carry a secret.
	Env []string
	// Stdin is written to the command's stdin, which is then closed. It is
	// how a credential reaches the dump: nothing the daemon records about
	// the container holds it.
	Stdin []byte
	// Input follows Stdin on the command's stdin: the stream a restore
	// loads into the database. Stdin is closed only once Input has ended
	// with io.EOF; any other end kills the container, so a cut stream can
	// never look like a complete one to the tool reading it. Close closes
	// Input when it is an io.Closer.
	Input io.Reader
	// StderrTail is how much of stderr the error and Stderr keep; zero is
	// 4 KiB. A load reads more of it, to judge the errors a tool reports
	// without failing.
	StderrTail int
	User       string
	Labels     map[string]string
	Limits     Limits
}

// ExitStatus is how a backup container ended.
type ExitStatus struct {
	Code      int64
	OOMKilled bool
}

func (e ExitStatus) String() string {
	if e.OOMKilled {
		return fmt.Sprintf("killed for exceeding its memory limit (exit %d)", e.Code)
	}
	return fmt.Sprintf("exit %d", e.Code)
}

// BackupContainer is a backup container found by its labels.
type BackupContainer struct {
	ID      string
	Name    string
	Role    string
	Labels  map[string]string
	Running bool
}

func checkLabels(labels map[string]string) error {
	for k := range labels {
		if !strings.HasPrefix(k, LabelBackupPrefix) || k == LabelBackupRole || k == LabelBackupVolume || k == LabelBackupFollow {
			return fmt.Errorf("backup container: label %q is not the caller's", k)
		}
	}
	return nil
}

func withLabels(labels map[string]string, extra ...string) map[string]string {
	out := make(map[string]string, len(labels)+len(extra)/2)
	for k, v := range labels {
		out[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i]] = extra[i+1]
	}
	return out
}

func randomName(prefix string) string {
	var b [6]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func validTarget(t string) bool {
	return path.IsAbs(t) && path.Clean(t) == t && t != "/"
}

// StartHelper creates, attaches and starts a helper. Nothing it creates
// outlives a failure here; after success the caller owns the Helper and
// must Remove it.
func (c *Client) StartHelper(ctx context.Context, spec HelperSpec) (*Helper, error) {
	if !c.available {
		return nil, errors.New("Docker is not available")
	}
	if spec.Image == "" || len(spec.Entrypoint) == 0 {
		return nil, errors.New("backup helper: image and entrypoint are required")
	}
	if err := spec.Limits.validate(); err != nil {
		return nil, err
	}
	if err := checkLabels(spec.Labels); err != nil {
		return nil, err
	}
	for _, cp := range spec.Caps {
		if !slices.Contains(helperCaps, cp) {
			return nil, fmt.Errorf("backup helper: capability %q is not allowed", cp)
		}
	}

	name := randomName("odac-bk-")
	var mounts []mount.Mount
	targets := map[string]bool{}
	for _, m := range spec.Mounts {
		if !validTarget(m.Target) || targets[m.Target] {
			return nil, fmt.Errorf("backup helper: invalid mount target %q", m.Target)
		}
		if !path.IsAbs(m.Source) {
			return nil, fmt.Errorf("backup helper: mount source %q is not absolute", m.Source)
		}
		targets[m.Target] = true
		src := m.Source
		if !m.HostSource {
			src = c.ResolveHostPath(src)
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: src, Target: m.Target, ReadOnly: m.ReadOnly})
	}

	labels := withLabels(spec.Labels, LabelBackupRole, RoleHelper)
	if spec.Follow {
		labels[LabelBackupFollow] = "true"
	}
	var vol string
	if s := spec.Share; s != nil {
		if !validTarget(s.Target) || targets[s.Target] {
			return nil, fmt.Errorf("backup helper: invalid share target %q", s.Target)
		}
		opts, err := s.driverOpts()
		if err != nil {
			return nil, err
		}
		vol = name + "-share"
		if _, err := c.api.VolumeCreate(ctx, volume.CreateOptions{
			Name:       vol,
			Driver:     "local",
			DriverOpts: opts,
			Labels:     withLabels(spec.Labels, LabelBackupRole, RoleShare),
		}); err != nil {
			return nil, fmt.Errorf("backup helper: create share volume: %w", err)
		}
		labels[LabelBackupVolume] = vol
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: vol, Target: s.Target})
	}

	netMode := container.NetworkMode("none")
	if spec.Network {
		if err := c.ensureNetwork(ctx, BackupNetwork); err != nil {
			c.removeVolume(vol)
			return nil, err
		}
		netMode = container.NetworkMode(BackupNetwork)
	}
	logCfg := container.LogConfig{Type: "none"}
	if spec.Follow {
		logCfg = container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": helperLogSize, "max-file": "1"}}
	}
	grace := int(stopGrace / time.Second)
	cfg := &container.Config{
		Image:        spec.Image,
		Entrypoint:   strslice.StrSlice(spec.Entrypoint[:1]),
		Cmd:          strslice.StrSlice(spec.Entrypoint[1:]),
		User:         "0:0",
		Labels:       labels,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
		StopTimeout:  &grace,
		// The image's health check probes the server's API, which a helper
		// does not serve.
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
	}
	host := &container.HostConfig{
		Mounts:         mounts,
		NetworkMode:    netMode,
		CapDrop:        []string{"ALL"},
		CapAdd:         spec.Caps,
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": tmpfsSize},
		LogConfig:      logCfg,
		Resources:      spec.Limits.resources(),
	}
	h, err := c.startAttached(ctx, cfg, host, name, true)
	if err != nil {
		c.removeVolume(vol)
		return nil, fmt.Errorf("backup helper: %w", err)
	}
	h.volume = vol
	return &Helper{attached: h}, nil
}

// StartDump creates, attaches and starts a dump container. Its output
// never reaches a log: the container runs with logging off, so the
// database's contents exist only in the stream the caller reads.
func (c *Client) StartDump(ctx context.Context, spec DumpSpec) (*Dump, error) {
	if !c.available {
		return nil, errors.New("Docker is not available")
	}
	if spec.Image == "" || spec.App == "" || len(spec.Cmd) == 0 {
		return nil, errors.New("backup dump: image, app container and command are required")
	}
	if err := spec.Limits.validate(); err != nil {
		return nil, err
	}
	if err := checkLabels(spec.Labels); err != nil {
		return nil, err
	}
	grace := int(stopGrace / time.Second)
	stdin := len(spec.Stdin) > 0 || spec.Input != nil
	cfg := &container.Config{
		Image:        spec.Image,
		Entrypoint:   strslice.StrSlice(spec.Cmd[:1]),
		Cmd:          strslice.StrSlice(spec.Cmd[1:]),
		Env:          spec.Env,
		User:         spec.User,
		Labels:       withLabels(spec.Labels, LabelBackupRole, RoleDump),
		AttachStdin:  stdin,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    stdin,
		StdinOnce:    stdin,
		StopTimeout:  &grace,
		Healthcheck:  &container.HealthConfig{Test: []string{"NONE"}},
	}
	host := &container.HostConfig{
		NetworkMode:    container.NetworkMode("container:" + spec.App),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": tmpfsSize},
		LogConfig:      container.LogConfig{Type: "none"},
		Resources:      spec.Limits.resources(),
	}
	a, err := c.startAttached(ctx, cfg, host, randomName("odac-dump-"), stdin)
	if err != nil {
		return nil, fmt.Errorf("backup dump: %w", err)
	}
	if spec.StderrTail > 0 {
		a.stderr.setMax(spec.StderrTail)
	}
	d := &Dump{attached: a}
	if len(spec.Stdin) > 0 {
		_, err := a.hj.Conn.Write(spec.Stdin)
		if err == nil && spec.Input == nil {
			err = a.hj.CloseWrite()
		}
		if err != nil {
			a.Remove()
			return nil, fmt.Errorf("backup dump: write stdin: %w", err)
		}
	}
	if spec.Input != nil {
		d.input = spec.Input
		d.fed = make(chan struct{})
		go d.feed()
	}
	return d, nil
}

// feed copies Input to stdin. Only a stream that ended with io.EOF closes
// stdin; a failed one kills the container, which then exits non-zero.
func (d *Dump) feed() {
	defer close(d.fed)
	_, err := io.Copy(d.hj.Conn, d.input)
	if err == nil {
		err = d.hj.CloseWrite()
		if err == nil {
			return
		}
	}
	d.mu.Lock()
	d.inputErr = err
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	now := 0
	if kerr := d.c.api.ContainerStop(ctx, d.id, container.StopOptions{Timeout: &now}); kerr != nil && !cerrdefs.IsNotFound(kerr) {
		d.c.log.Error("Failed to kill backup container %s: %s", d.id, kerr.Error())
	}
}

// startAttached attaches before starting, so no output is missed, and
// removes the container again when either step fails. An I/O weight the
// host cannot apply fails the start itself (runc finds no io.weight in the
// container's cgroup when the disk's scheduler has no weights, which is
// most cloud disks), so that start is retried once without it and the
// client stops asking for one.
func (c *Client) startAttached(ctx context.Context, cfg *container.Config, host *container.HostConfig, name string, stdin bool) (*attached, error) {
	unweighted := *host
	unweighted.BlkioWeight = 0
	c.mu.Lock()
	if c.noIOWeight {
		host = &unweighted
	}
	c.mu.Unlock()
	a, err := c.createAttached(ctx, cfg, host, name, stdin)
	if err != nil && host.BlkioWeight != 0 && ioWeightUnsupported(err) {
		c.mu.Lock()
		c.noIOWeight = true
		c.mu.Unlock()
		c.log.Log("The host cannot weight container I/O; backup containers run without an I/O weight")
		return c.createAttached(ctx, cfg, &unweighted, name, stdin)
	}
	return a, err
}

func ioWeightUnsupported(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "io.weight") || strings.Contains(msg, "io.bfq.weight") || strings.Contains(msg, "blkio.weight")
}

func (c *Client) createAttached(ctx context.Context, cfg *container.Config, host *container.HostConfig, name string, stdin bool) (*attached, error) {
	resp, err := c.api.ContainerCreate(ctx, cfg, host, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*attached, error) {
		c.removeContainer(resp.ID)
		return nil, err
	}
	hj, err := c.api.ContainerAttach(ctx, resp.ID, container.AttachOptions{Stream: true, Stdin: stdin, Stdout: true, Stderr: true})
	if err != nil {
		return fail(err)
	}
	a := newAttached(c, resp.ID, hj)
	if err := c.api.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		a.hj.Close()
		return fail(err)
	}
	return a, nil
}

func (c *Client) removeContainer(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := c.api.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		c.log.Error("Failed to remove backup container %s: %s", id, err.Error())
	}
}

func (c *Client) removeVolume(name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := c.api.VolumeRemove(ctx, name, true); err != nil && !cerrdefs.IsNotFound(err) {
		c.log.Error("Failed to remove backup share volume %s: %s", name, err.Error())
	}
}

// attached is a started container whose stdout and stderr are being
// demultiplexed: stdout into a pipe, stderr into a bounded tail.
type attached struct {
	c      *Client
	id     string
	hj     types.HijackedResponse
	stdout *io.PipeReader
	stderr *tailBuffer
	demux  chan struct{}
	volume string

	closeOnce sync.Once
}

func newAttached(c *Client, id string, hj types.HijackedResponse) *attached {
	pr, pw := io.Pipe()
	a := &attached{c: c, id: id, hj: hj, stdout: pr, stderr: &tailBuffer{max: stderrTail}, demux: make(chan struct{})}
	go func() {
		defer close(a.demux)
		_, err := stdcopy.StdCopy(pw, a.stderr, hj.Reader)
		pw.CloseWithError(err)
	}()
	return a
}

// ID is the container's id.
func (a *attached) ID() string { return a.id }

// Stderr is the end of what the container wrote to stderr.
func (a *attached) Stderr() string { return a.stderr.String() }

func (a *attached) close() {
	a.closeOnce.Do(func() {
		a.hj.Close()
		a.stdout.Close()
	})
}

// Wait blocks until the container exits.
func (a *attached) Wait(ctx context.Context) (ExitStatus, error) {
	okC, errC := a.c.api.ContainerWait(ctx, a.id, container.WaitConditionNotRunning)
	var st ExitStatus
	select {
	case resp := <-okC:
		st.Code = resp.StatusCode
	case err := <-errC:
		return st, err
	}
	ictx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if info, err := a.c.api.ContainerInspect(ictx, a.id); err == nil && info.State != nil {
		st.OOMKilled = info.State.OOMKilled
	}
	return st, nil
}

// Stop asks the container to end (SIGTERM, then SIGKILL after stopGrace).
func (a *attached) Stop() {
	grace := int(stopGrace / time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), stopGrace+probeTimeout)
	defer cancel()
	if err := a.c.api.ContainerStop(ctx, a.id, container.StopOptions{Timeout: &grace}); err != nil && !cerrdefs.IsNotFound(err) {
		a.c.log.Error("Failed to stop backup container %s: %s", a.id, err.Error())
	}
}

// Remove detaches and removes the container, killing it if it still
// runs, and its share volume.
func (a *attached) Remove() {
	a.close()
	<-a.demux
	a.c.removeContainer(a.id)
	a.c.removeVolume(a.volume)
}

// Detach drops the connection and leaves the container running, which is
// how a server hands a run over to the one that replaces it.
func (a *attached) Detach() {
	a.close()
	<-a.demux
}

// Helper is a running odac-backup container.
type Helper struct{ *attached }

// Stdin is the helper's stdin. Close ends it without closing stdout,
// which is what the protocol does after the header or the last frame.
func (h *Helper) Stdin() io.WriteCloser { return helperStdin{h.attached} }

// Stdout is the helper's stdout. Close drops the whole connection.
func (h *Helper) Stdout() io.ReadCloser { return helperStdout{h.attached} }

type helperStdin struct{ a *attached }

func (s helperStdin) Write(p []byte) (int, error) { return s.a.hj.Conn.Write(p) }
func (s helperStdin) Close() error                { return s.a.hj.CloseWrite() }

type helperStdout struct{ a *attached }

func (s helperStdout) Read(p []byte) (int, error) { return s.a.stdout.Read(p) }
func (s helperStdout) Close() error               { s.a.close(); return nil }

// Dump is a running dump container. Read returns its stdout and ends
// cleanly only when the dump exited 0: any other end is an error naming
// the exit and the end of its stderr, so a dump that failed half-way is
// never taken for a complete one.
type Dump struct {
	*attached
	once sync.Once
	err  error

	input    io.Reader
	fed      chan struct{}
	mu       sync.Mutex
	inputErr error
}

func (d *Dump) Read(p []byte) (int, error) {
	n, err := d.stdout.Read(p)
	if errors.Is(err, io.EOF) {
		d.once.Do(func() { d.err = d.finish() })
		if d.err != nil {
			return n, d.err
		}
	}
	return n, err
}

func (d *Dump) finish() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
	defer cancel()
	st, err := d.Wait(ctx)
	if err != nil {
		return fmt.Errorf("dump: wait: %w", err)
	}
	if d.fed != nil {
		<-d.fed
		d.mu.Lock()
		ierr := d.inputErr
		d.mu.Unlock()
		if ierr != nil {
			return fmt.Errorf("load: the stream failed: %w", ierr)
		}
	}
	if st.Code != 0 || st.OOMKilled {
		msg := strings.TrimSpace(d.Stderr())
		if msg == "" {
			return fmt.Errorf("dump %s", st)
		}
		return fmt.Errorf("dump %s: %s", st, msg)
	}
	return nil
}

// Close stops reading and removes the container, which ends a dump that
// is blocked on its output, and with Input it closes Input and waits for
// the copy to stop.
func (d *Dump) Close() error {
	d.Remove()
	if d.fed != nil {
		if c, ok := d.input.(io.Closer); ok {
			c.Close()
		}
		<-d.fed
	}
	return nil
}

// BackupContainers lists every backup container, running or not, so a
// server can take over or clear what the one before it left.
func (c *Client) BackupContainers() ([]BackupContainer, error) {
	if !c.available {
		return nil, errors.New("Docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	list, err := c.api.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", LabelBackupRole))})
	if err != nil {
		return nil, err
	}
	out := make([]BackupContainer, 0, len(list))
	for _, s := range list {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		out = append(out, BackupContainer{
			ID:      s.ID,
			Name:    name,
			Role:    s.Labels[LabelBackupRole],
			Labels:  s.Labels,
			Running: s.State == container.StateRunning,
		})
	}
	return out, nil
}

// FollowHelper replays a followable helper's stdout from its first line
// and keeps reading until the helper exits.
func (c *Client) FollowHelper(ctx context.Context, id string) (io.ReadCloser, error) {
	rc, err := c.api.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, io.Discard, rc)
		rc.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// WaitBackupContainer waits for a container found by BackupContainers.
func (c *Client) WaitBackupContainer(ctx context.Context, id string) (ExitStatus, error) {
	return (&attached{c: c, id: id}).Wait(ctx)
}

// RemoveBackupContainer removes a backup container, killing it if it
// runs, and the share volume it mounts.
func (c *Client) RemoveBackupContainer(b BackupContainer) {
	c.removeContainer(b.ID)
	c.removeVolume(b.Labels[LabelBackupVolume])
}

// SweepBackupVolumes removes share volumes no container uses any more,
// which a server that died between removing a helper and its volume
// leaves behind. A volume in use is refused by the daemon and kept.
func (c *Client) SweepBackupVolumes() {
	if !c.available {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	list, err := c.api.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs(filters.Arg("label", LabelBackupRole+"="+RoleShare))})
	if err != nil {
		c.log.Error("Failed to list backup share volumes: %s", err.Error())
		return
	}
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if err := c.api.VolumeRemove(ctx, v.Name, false); err != nil && !cerrdefs.IsConflict(err) && !cerrdefs.IsNotFound(err) {
			c.log.Error("Failed to remove backup share volume %s: %s", v.Name, err.Error())
		}
	}
}

// StderrTail keeps the last bytes written to it: the end of a backup
// process's stderr, which is where a crash says why. The zero value keeps
// nothing; use NewStderrTail.
type StderrTail = tailBuffer

// NewStderrTail returns a StderrTail that keeps max bytes.
func NewStderrTail(max int) *StderrTail { return &tailBuffer{max: max} }

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = bytes.Clone(t.buf[over:])
	}
	return len(p), nil
}

func (t *tailBuffer) setMax(n int) {
	t.mu.Lock()
	t.max = n
	t.mu.Unlock()
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// AppState is what a backup reads from an app's running container: the
// image it runs, which a dump runs too so the tool matches the server's
// version, and its environment with the image's defaults, which is where
// a database's credentials are.
type AppState struct {
	Running     bool
	ImageID     string
	Cmd         []string
	Env         map[string]string
	MemoryLimit int64
}

// BackupAppState inspects an app's container for a backup. A container
// that does not exist reads as not running, not as an error: a stopped
// app is backed up from its files.
func (c *Client) BackupAppState(ctx context.Context, name string) (AppState, error) {
	if !c.available {
		return AppState{}, errors.New("Docker is not available")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	info, err := c.api.ContainerInspect(ctx, name)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return AppState{}, nil
		}
		return AppState{}, err
	}
	st := AppState{ImageID: info.Image, Env: map[string]string{}}
	if info.State != nil {
		st.Running = info.State.Running
	}
	if info.Config != nil {
		st.Cmd = append([]string(nil), info.Config.Cmd...)
		for _, kv := range info.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			st.Env[k] = v
		}
	}
	if info.HostConfig != nil {
		st.MemoryLimit = info.HostConfig.Memory
	}
	return st, nil
}
