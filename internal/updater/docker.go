package updater

import (
	"context"
	"errors"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// ContainerInfo carries the inspect fields the updater reads. Name keeps the
// leading slash exactly as Docker reports it (Node strips it only in
// #resolveSelfName; execute() logs it slash-included).
type ContainerInfo struct {
	ID            string
	Name          string
	Env           []string
	Binds         []string
	RestartPolicy string
	Running       bool
	Image         string // the image ID (sha256:…), not the tag
	PortBindings  nat.PortMap
	Mounts        []MountPoint
	LogConfig     LogConfig
}

// MountPoint is the named-volume fallback #resolveHostBind walks.
type MountPoint struct {
	Name        string
	Source      string
	Destination string
}

// LogConfig mirrors HostConfig.LogConfig (log driver + its options). Zero
// value means "daemon default", same as Docker's own semantics.
type LogConfig struct {
	Type   string
	Config map[string]string
}

// CreateOptions mirrors the dockerode createContainer options the updater
// builds (execute()'s createOptions, the git sidecar and the swap runner).
type CreateOptions struct {
	Name          string // empty = Docker-generated (sidecar/runner containers)
	Image         string
	Env           []string
	Cmd           []string
	Binds         []string
	Privileged    bool
	CapAdd        []string
	RestartPolicy string
	Tty           bool
	NetworkMode   string
	PidMode       string
	AutoRemove    bool
	PortBindings  nat.PortMap
	LogConfig     LogConfig // zero value = daemon default (sidecar/runner containers)
}

// Docker is the updater's view of the daemon, shaped 1:1 after the dockerode
// calls Updater.js makes so the stateful test fake mirrors the jest one.
// Node shelled out to the docker CLI for pull and cp; the Go port unifies on
// the SDK (sanctioned by lifecycle.md's migration notes).
type Docker interface {
	Inspect(name string) (ContainerInfo, error)
	Create(opts CreateOptions) (id string, err error)
	Start(name string) error
	Stop(name string) error
	Rename(name, newName string) error
	Remove(name string, force bool) error
	UpdateRestartPolicy(name, policy string) error
	// Wait blocks until the container stops and returns its exit code.
	Wait(name string) (int64, error)
	// Logs returns the raw log stream (tail "" = all).
	Logs(name string, follow bool, tail string) (io.ReadCloser, error)
	Pull(imageRef string) error
	// ImageID returns the ID of a local image tag (getImage(...).inspect().Id).
	ImageID(imageRef string) (string, error)
	// ReadFile returns a file from the container's filesystem (docker cp).
	ReadFile(name, path string) ([]byte, error)
}

// isNotFound reports Docker's 404 (Node: e.statusCode === 404). The SDK
// client and the test fakes both wrap containerd's errdefs sentinel.
func isNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

// ConnectDocker builds the SDK-backed Docker implementation. The updater owns
// its own client, like Node's Updater constructs its own dockerode instance.
func ConnectDocker() (Docker, error) {
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &sdkDocker{api: api}, nil
}

type sdkDocker struct{ api *client.Client }

func (d *sdkDocker) Inspect(name string) (ContainerInfo, error) {
	resp, err := d.api.ContainerInspect(context.Background(), name)
	if err != nil {
		return ContainerInfo{}, err
	}
	info := ContainerInfo{ID: resp.ID, Name: resp.Name, Image: resp.Image}
	if resp.Config != nil {
		info.Env = resp.Config.Env
	}
	if resp.HostConfig != nil {
		info.Binds = resp.HostConfig.Binds
		info.RestartPolicy = string(resp.HostConfig.RestartPolicy.Name)
		info.PortBindings = resp.HostConfig.PortBindings
		info.LogConfig = LogConfig{Type: resp.HostConfig.LogConfig.Type, Config: resp.HostConfig.LogConfig.Config}
	}
	if resp.State != nil {
		info.Running = resp.State.Running
	}
	for _, m := range resp.Mounts {
		info.Mounts = append(info.Mounts, MountPoint{Name: m.Name, Source: m.Source, Destination: string(m.Destination)})
	}
	return info, nil
}

func (d *sdkDocker) Create(opts CreateOptions) (string, error) {
	cfg := &container.Config{
		Image: opts.Image,
		Env:   opts.Env,
		Cmd:   strslice.StrSlice(opts.Cmd),
		Tty:   opts.Tty,
	}
	host := &container.HostConfig{
		Binds:        opts.Binds,
		Privileged:   opts.Privileged,
		CapAdd:       strslice.StrSlice(opts.CapAdd),
		AutoRemove:   opts.AutoRemove,
		NetworkMode:  container.NetworkMode(opts.NetworkMode),
		PidMode:      container.PidMode(opts.PidMode),
		PortBindings: opts.PortBindings,
		LogConfig:    container.LogConfig{Type: opts.LogConfig.Type, Config: opts.LogConfig.Config},
	}
	if opts.RestartPolicy != "" {
		host.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyMode(opts.RestartPolicy)}
	}
	resp, err := d.api.ContainerCreate(context.Background(), cfg, host, &network.NetworkingConfig{}, nil, opts.Name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (d *sdkDocker) Start(name string) error {
	return d.api.ContainerStart(context.Background(), name, container.StartOptions{})
}

func (d *sdkDocker) Stop(name string) error {
	return d.api.ContainerStop(context.Background(), name, container.StopOptions{})
}

func (d *sdkDocker) Rename(name, newName string) error {
	return d.api.ContainerRename(context.Background(), name, newName)
}

func (d *sdkDocker) Remove(name string, force bool) error {
	return d.api.ContainerRemove(context.Background(), name, container.RemoveOptions{Force: force})
}

func (d *sdkDocker) UpdateRestartPolicy(name, policy string) error {
	_, err := d.api.ContainerUpdate(context.Background(), name, container.UpdateConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(policy)},
	})
	return err
}

func (d *sdkDocker) Wait(name string) (int64, error) {
	waitCh, errCh := d.api.ContainerWait(context.Background(), name, container.WaitConditionNotRunning)
	select {
	case resp := <-waitCh:
		return resp.StatusCode, nil
	case err := <-errCh:
		return 0, err
	}
}

func (d *sdkDocker) Logs(name string, follow bool, tail string) (io.ReadCloser, error) {
	return d.api.ContainerLogs(context.Background(), name, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: follow, Tail: tail,
	})
}

func (d *sdkDocker) Pull(imageRef string) error {
	rc, err := d.api.ImagePull(context.Background(), imageRef, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc) // pull completes when the stream ends
	return err
}

func (d *sdkDocker) ImageID(imageRef string) (string, error) {
	resp, _, err := d.api.ImageInspectWithRaw(context.Background(), imageRef)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (d *sdkDocker) ReadFile(name, path string) ([]byte, error) {
	rc, _, err := d.api.CopyFromContainer(context.Background(), name, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return firstTarFile(rc)
}

// unavailableDocker stands in when no SDK client could be constructed
// (malformed DOCKER_HOST-style env). Every call fails like an unreachable
// daemon would in Node.
type unavailableDocker struct{}

var errDockerUnavailable = errors.New("docker is not available")

func (unavailableDocker) Inspect(string) (ContainerInfo, error) {
	return ContainerInfo{}, errDockerUnavailable
}
func (unavailableDocker) Create(CreateOptions) (string, error)     { return "", errDockerUnavailable }
func (unavailableDocker) Start(string) error                       { return errDockerUnavailable }
func (unavailableDocker) Stop(string) error                        { return errDockerUnavailable }
func (unavailableDocker) Rename(string, string) error              { return errDockerUnavailable }
func (unavailableDocker) Remove(string, bool) error                { return errDockerUnavailable }
func (unavailableDocker) UpdateRestartPolicy(string, string) error { return errDockerUnavailable }
func (unavailableDocker) Wait(string) (int64, error)               { return 0, errDockerUnavailable }
func (unavailableDocker) Logs(string, bool, string) (io.ReadCloser, error) {
	return nil, errDockerUnavailable
}
func (unavailableDocker) Pull(string) error              { return errDockerUnavailable }
func (unavailableDocker) ImageID(string) (string, error) { return "", errDockerUnavailable }
func (unavailableDocker) ReadFile(string, string) ([]byte, error) {
	return nil, errDockerUnavailable
}
