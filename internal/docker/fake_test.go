package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// notFoundErr mimics the SDK's 404 errors (client.IsErrNotFound matches
// errdefs.IsNotFound, which unwraps).
type notFoundErr struct{ msg string }

func (e notFoundErr) Error() string { return e.msg }
func (e notFoundErr) NotFound()     {}

// createCall records one ContainerCreate invocation.
type createCall struct {
	Config     *container.Config
	HostConfig *container.HostConfig
	Name       string
	ID         string
}

// fakeAPI implements the API interface with programmable behavior.
type fakeAPI struct {
	mu sync.Mutex

	pingErr error

	// createErr fails ContainerCreate (daemon refusals).
	createErr error

	// info answers docker-info calls; infoErr wins when set.
	info    system.Info
	infoErr error

	// images that "exist locally".
	images map[string]image.InspectResponse
	// pull stream body per image (JSON lines); nil → not pullable.
	pullBodies map[string]string
	pulled     []string

	networks []string
	// netCreateOpts records the options each network was created with, so a
	// test can assert the `internal` flag and not just the name.
	netCreateOpts map[string]network.CreateOptions
	// preInternal marks pre-seeded `networks` entries as internal, letting a
	// test exercise the "exists but is not internal" branch.
	preInternal map[string]bool
	created     []createCall
	started     []string
	stopped     []string
	removed     []string
	renamed     [][2]string
	nextID      int

	// waitCode per container ID (default 0).
	waitCodes map[string]int64
	// log output per container ID (mux'd on read).
	logOutputs map[string]string

	inspects map[string]container.InspectResponse

	execCmds    []string
	execOutputs map[string]string // command -> stdout
	execCodes   map[string]int    // command -> exit code

	statsBody string

	netConnects    [][2]string
	netDisconnects [][2]string

	removedImages []string
	prunedImages  bool
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		images:      map[string]image.InspectResponse{},
		pullBodies:  map[string]string{},
		waitCodes:   map[string]int64{},
		logOutputs:  map[string]string{},
		inspects:    map[string]container.InspectResponse{},
		execOutputs: map[string]string{},
		execCodes:   map[string]int{},
	}
}

func (f *fakeAPI) Ping(context.Context) (types.Ping, error) {
	return types.Ping{}, f.pingErr
}

func (f *fakeAPI) Info(context.Context) (system.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, f.infoErr
}

func (f *fakeAPI) ContainerCreate(_ context.Context, cfg *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	f.nextID++
	id := fmt.Sprintf("ctr%d", f.nextID)
	f.created = append(f.created, createCall{Config: cfg, HostConfig: hc, Name: name, ID: id})
	return container.CreateResponse{ID: id}, nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return nil
}

func (f *fakeAPI) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	if _, ok := f.inspects[id]; !ok {
		return notFoundErr{"No such container: " + id}
	}
	return nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if resp, ok := f.inspects[id]; ok {
		return resp, nil
	}
	return container.InspectResponse{}, notFoundErr{"No such container: " + id}
}

func (f *fakeAPI) ContainerStatPath(_ context.Context, _, _ string) (container.PathStat, error) {
	return container.PathStat{}, notFoundErr{"stat: no such path"}
}

func (f *fakeAPI) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return []container.Summary{
		{ID: "abcdef0123456789", Names: []string{"/one"}, Image: "img", State: "running", Status: "Up", Created: 5},
	}, nil
}

func muxBytes(stdout string) []byte {
	var buf bytes.Buffer
	w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
	w.Write([]byte(stdout))
	return buf.Bytes()
}

func (f *fakeAPI) ContainerLogs(_ context.Context, id string, _ container.LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, ok := f.logOutputs[id]
	if !ok {
		out = ""
	}
	return io.NopCloser(bytes.NewReader(muxBytes(out))), nil
}

func (f *fakeAPI) ContainerWait(_ context.Context, id string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	waitCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	f.mu.Lock()
	code := f.waitCodes[id]
	f.mu.Unlock()
	waitCh <- container.WaitResponse{StatusCode: code}
	return waitCh, errCh
}

func (f *fakeAPI) ContainerRename(_ context.Context, id, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renamed = append(f.renamed, [2]string{id, newName})
	return nil
}

func (f *fakeAPI) ContainerStatsOneShot(context.Context, string) (container.StatsResponseReader, error) {
	return container.StatsResponseReader{
		Body: io.NopCloser(bytes.NewReader([]byte(f.statsBody))),
	}, nil
}

func (f *fakeAPI) ContainerAttach(context.Context, string, container.AttachOptions) (types.HijackedResponse, error) {
	server, client := net.Pipe()
	server.Close()
	return types.NewHijackedResponse(client, ""), nil
}

func (f *fakeAPI) ContainerExecCreate(_ context.Context, _ string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := ""
	if len(options.Cmd) == 3 {
		cmd = options.Cmd[2]
	}
	f.execCmds = append(f.execCmds, cmd)
	return container.ExecCreateResponse{ID: "exec:" + cmd}, nil
}

func (f *fakeAPI) ContainerExecAttach(_ context.Context, execID string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.mu.Lock()
	cmd := execID[len("exec:"):]
	out := f.execOutputs[cmd]
	f.mu.Unlock()

	server, cl := net.Pipe()
	go func() {
		server.Write(muxBytes(out))
		server.Close()
	}()
	return types.HijackedResponse{Conn: cl, Reader: bufio.NewReader(cl)}, nil
}

func (f *fakeAPI) ContainerExecInspect(_ context.Context, execID string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := execID[len("exec:"):]
	return container.ExecInspect{ExitCode: f.execCodes[cmd]}, nil
}

func (f *fakeAPI) ContainerExecResize(context.Context, string, container.ResizeOptions) error {
	return nil
}

func (f *fakeAPI) ImageInspectWithRaw(_ context.Context, id string) (image.InspectResponse, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if resp, ok := f.images[id]; ok {
		return resp, nil, nil
	}
	return image.InspectResponse{}, nil, notFoundErr{"No such image: " + id}
}

func (f *fakeAPI) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	body, ok := f.pullBodies[ref]
	if !ok {
		body = `{"status":"Pulling from library"}` + "\n"
	}
	// The image becomes "local" after a pull.
	f.images[ref] = image.InspectResponse{}
	return io.NopCloser(bytes.NewReader([]byte(body))), nil
}

func (f *fakeAPI) ImageRemove(_ context.Context, id string, _ image.RemoveOptions) ([]image.DeleteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[id]; !ok {
		return nil, notFoundErr{"No such image: " + id}
	}
	delete(f.images, id)
	f.removedImages = append(f.removedImages, id)
	return []image.DeleteResponse{{Deleted: id}}, nil
}

func (f *fakeAPI) ImagesPrune(context.Context, filters.Args) (image.PruneReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunedImages = true
	return image.PruneReport{}, nil
}

func (f *fakeAPI) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []network.Summary
	for _, n := range f.networks {
		internal := f.preInternal[n]
		if opts, ok := f.netCreateOpts[n]; ok {
			internal = opts.Internal
		}
		out = append(out, network.Summary{Name: n, Internal: internal})
	}
	return out, nil
}

func (f *fakeAPI) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks = append(f.networks, name)
	if f.netCreateOpts == nil {
		f.netCreateOpts = map[string]network.CreateOptions{}
	}
	f.netCreateOpts[name] = opts
	return network.CreateResponse{ID: name}, nil
}

func (f *fakeAPI) NetworkConnect(_ context.Context, networkID, containerID string, _ *network.EndpointSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netConnects = append(f.netConnects, [2]string{networkID, containerID})
	return nil
}

func (f *fakeAPI) NetworkDisconnect(_ context.Context, networkID, containerID string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netDisconnects = append(f.netDisconnects, [2]string{networkID, containerID})
	return nil
}
