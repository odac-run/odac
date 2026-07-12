// Package docker is the Go port of server/src/Container.js (plus
// Container/Builder.js in builder.go): app container lifecycle, image
// management, git clone/fetch sandboxes and the native two-stage builder,
// backed by the official Docker SDK instead of dockerode.
//
// API style: like Node, methods are context-less (the orchestrator has no
// cancellation story — the watchdog restarts the whole process) and most
// getters swallow errors into zero values, logging anything that is not a
// 404. Availability is probed once at construction: a host whose Docker
// engine is down when odac-server starts stays "unavailable" until the next
// restart, exactly like Node's constructor-time ping.
//
// Deviations from Node (deliberate): Env lists are assembled in sorted key
// order (Node: object insertion order — Docker does not care); the
// availability flag lives on the Client, not in config.container (contract
// 0.6: ephemeral runtime state must not hit the config file).
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"odac/internal/applog"
	"odac/internal/logx"
	"odac/internal/ports"
)

// networkName is the shared bridge network every ODAC app joins.
const networkName = "odac-network"

// API is the narrow slice of the Docker SDK client the orchestrator uses.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	Ping(ctx context.Context) (types.Ping, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerLogs(ctx context.Context, containerID string, options container.LogsOptions) (io.ReadCloser, error)
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerRename(ctx context.Context, containerID, newContainerName string) error
	ContainerStatsOneShot(ctx context.Context, containerID string) (container.StatsResponseReader, error)
	ContainerAttach(ctx context.Context, containerID string, options container.AttachOptions) (types.HijackedResponse, error)
	ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error
	ImageInspectWithRaw(ctx context.Context, imageID string) (image.InspectResponse, []byte, error)
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
}

// Mount is one volume mapping. Container may carry a ":ro" suffix.
type Mount struct{ Host, Container string }

// Device is one host-device mapping; an empty Container defaults to Host.
type Device struct{ Host, Container string }

// RunOptions ports Container.runApp's options object.
type RunOptions struct {
	Image string
	// Ports holds the app's port entries (decoded config JSON). Only
	// published entries become Docker PortBindings; proxy-routed ones are
	// routing metadata and are skipped (defense in depth — App filters too).
	Ports   []map[string]any
	Volumes []Mount
	Devices []Device
	Env     map[string]string
	Cmd     []string
	User    string
	// Privileged enables Docker Privileged mode. SECURITY: full host
	// device/kernel access; CLI-only escape hatch.
	Privileged bool
}

// BuildLog is the phase-aware build log control the container operations
// stream into; *applog.BuildControl satisfies it. Nil is allowed everywhere.
type BuildLog interface {
	io.Writer
	StartPhase(name string)
	EndPhase(name string, success bool)
}

// HostPathResolver converts a container-internal path to the host-native
// path Docker needs (DooD). Exposed so appmgr can reuse it in list().
type HostPathResolver interface {
	ResolveHostPath(localPath string) string
}

// Client wraps the Docker API with Container.js semantics.
type Client struct {
	api       API
	log       *logx.Logger
	available bool

	// hostRoot is ODAC_HOST_ROOT: the host path that the orchestrator's
	// /app directory is bind-mounted from (empty on bare metal).
	hostRoot string

	// logsRoot feeds the builder's self-created loggers (<base>/logs).
	logsRoot string

	mu           sync.Mutex
	activeBuilds map[string]bool
	buildLoggers map[string]*applog.Logger

	// appNames resolves managed app names for CreateTerminalSession
	// (injected by main; see SetAppNames).
	appNames func() []string
}

// Options configures New.
type Options struct {
	// HostRoot is the ODAC_HOST_ROOT env value (may be empty).
	HostRoot string
	// LogsRoot is where per-app logs live, e.g. <baseDir>/logs.
	LogsRoot string
}

// Connect builds a Client over the real Docker engine (env-configured, API
// version negotiated) and probes availability once.
func Connect(opts Options) (*Client, error) {
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return New(api, opts), nil
}

// New builds a Client over any API implementation and probes availability
// once, mirroring Node's constructor-time ping.
func New(api API, opts Options) *Client {
	c := &Client{
		api:          api,
		log:          logx.New("Container"),
		hostRoot:     opts.HostRoot,
		logsRoot:     opts.LogsRoot,
		activeBuilds: map[string]bool{},
		buildLoggers: map[string]*applog.Logger{},
	}
	if _, err := api.Ping(context.Background()); err == nil {
		c.available = true
		c.log.Log("Docker is available")
	} else {
		c.log.Error("Docker is not available")
	}
	return c
}

// Available reports whether Docker answered the construction-time ping.
func (c *Client) Available() bool { return c.available }

// ResolveHostPath ports Container.resolveHostPath (DooD support): a
// container-internal /app path is rewritten under ODAC_HOST_ROOT so the
// host's Docker daemon can bind-mount it.
func (c *Client) ResolveHostPath(localPath string) string {
	if c.hostRoot == "" {
		return localPath
	}
	if strings.HasPrefix(localPath, "/app") {
		return filepath.Join(c.hostRoot, localPath[4:])
	}
	if !filepath.IsAbs(localPath) {
		if abs, err := filepath.Abs(localPath); err == nil && strings.HasPrefix(abs, "/app") {
			return filepath.Join(c.hostRoot, abs[4:])
		}
	}
	return localPath
}

// RegisterBuildLogger / UnregisterBuildLogger / SubscribeToBuildLogs /
// GetLastBuildLog port the build-log registry the Hub reads through.

func (c *Client) RegisterBuildLogger(appName string, logger *applog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buildLoggers[appName] = logger
}

func (c *Client) UnregisterBuildLogger(appName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.buildLoggers, appName)
}

// SubscribeToBuildLogs attaches cb to the app's registered build logger;
// nil unsubscribe when no build logger is registered.
func (c *Client) SubscribeToBuildLogs(appName string, cb func(applog.Entry)) func() {
	c.mu.Lock()
	logger := c.buildLoggers[appName]
	c.mu.Unlock()
	if logger == nil {
		return nil
	}
	return logger.Subscribe(cb, applog.Build)
}

// GetLastBuildLog reads the app's most recent build log from disk ("" when
// none or unreadable).
func (c *Client) GetLastBuildLog(appName string) string {
	logger := applog.New(c.logsRoot, appName)
	if err := logger.Init(); err != nil {
		return ""
	}
	return logger.ReadLastBuildLog()
}

// ensureNetwork makes sure the named bridge network exists. Best-effort
// like Node: failures are logged, not returned.
func (c *Client) ensureNetwork(ctx context.Context, name string) {
	networks, err := c.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		c.log.Error("Failed to ensure network %s: %s", name, err.Error())
		return
	}
	for _, n := range networks {
		if n.Name == name {
			return
		}
	}
	c.log.Log("Creating network %s...", name)
	if _, err := c.api.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge"}); err != nil {
		c.log.Error("Failed to ensure network %s: %s", name, err.Error())
	}
}

// EnsureImage pulls the image when it is missing locally, streaming pull
// progress into logw (Node's followProgress rendering: raw `stream` events
// verbatim, `status [progress]` lines otherwise). Safe to call repeatedly.
func (c *Client) EnsureImage(imageName string, logw io.Writer) error {
	ctx := context.Background()
	if _, _, err := c.api.ImageInspectWithRaw(ctx, imageName); err == nil {
		if logw != nil {
			fmt.Fprintf(logw, "Image %s already exists locally.\n", imageName)
		}
		return nil
	}

	c.log.Log("Pulling image %s...", imageName)
	if logw != nil {
		fmt.Fprintf(logw, "Pulling image %s...\n", imageName)
	}

	rc, err := c.api.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return c.pullFailed(imageName, logw, err)
	}
	defer rc.Close()

	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Stream   string `json:"stream"`
			Status   string `json:"status"`
			Progress string `json:"progress"`
			Error    string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return c.pullFailed(imageName, logw, err)
		}
		if msg.Error != "" {
			return c.pullFailed(imageName, logw, fmt.Errorf("%s", msg.Error))
		}
		if logw == nil {
			continue
		}
		if msg.Stream != "" {
			io.WriteString(logw, msg.Stream)
		} else if msg.Status != "" {
			line := msg.Status
			if msg.Progress != "" {
				line += " " + msg.Progress
			}
			io.WriteString(logw, line+"\n")
		}
	}

	if logw != nil {
		fmt.Fprintf(logw, "Image %s pulled successfully.\n", imageName)
	}
	c.log.Log("Image %s pulled successfully.", imageName)
	return nil
}

func (c *Client) pullFailed(imageName string, logw io.Writer, err error) error {
	c.log.Error("Failed to pull image %s: %s", imageName, err.Error())
	if logw != nil {
		fmt.Fprintf(logw, "Failed to pull image %s: %s\n", imageName, err.Error())
	}
	return err
}

// RunApp ports Container.runApp: force-replace any container with this
// name, assemble Binds/Devices/PortBindings, ensure the shared network and
// image, create and start. Only published port entries reach Docker; public
// ones bind every interface, the rest loopback.
func (c *Client) RunApp(name string, options RunOptions, buildLog BuildLog) error {
	if !c.available {
		return nil
	}
	ctx := context.Background()

	c.Remove(name)

	var binds []string
	for _, vol := range options.Volumes {
		binds = append(binds, c.ResolveHostPath(vol.Host)+":"+vol.Container)
	}

	portBindings := nat.PortMap{}
	exposedPorts := nat.PortSet{}
	for _, entry := range options.Ports {
		// Defense in depth: a proxy-routed entry has no host binding to publish.
		if !ports.IsPublished(entry) {
			continue
		}
		portKey := nat.Port(jsString(entry["container"]) + "/tcp")
		if ports.IsPublic(entry) {
			c.log.Log("Publishing %s port %s on every interface (public).", name, jsString(entry["host"]))
		}
		// Append: Docker takes a list of host bindings per container port, so a
		// single container port may be published on several host ports.
		binding := nat.PortBinding{HostIP: ports.BindIP(entry), HostPort: jsString(entry["host"])}
		portBindings[portKey] = append(portBindings[portKey], binding)
		exposedPorts[portKey] = struct{}{}
	}

	var devices []container.DeviceMapping
	for _, dev := range options.Devices {
		inContainer := dev.Container
		if inContainer == "" {
			inContainer = dev.Host
		}
		devices = append(devices, container.DeviceMapping{
			PathOnHost:        dev.Host,
			PathInContainer:   inContainer,
			CgroupPermissions: "rwm",
		})
	}

	c.ensureNetwork(ctx, networkName)

	c.log.Log("Starting app container %s (%s)...", name, options.Image)
	if buildLog != nil {
		buildLog.StartPhase("pull_image")
	}
	if err := c.EnsureImage(options.Image, writerOrNil(buildLog)); err != nil {
		c.log.Error("Failed to start app container %s: %s", name, err.Error())
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_image", true)
		buildLog.StartPhase("start_new_container")
	}

	cfg := &container.Config{
		Image:        options.Image,
		Env:          envList(options.Env),
		ExposedPorts: exposedPorts,
		Cmd:          options.Cmd,
		User:         options.User,
	}
	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		Binds:         binds,
		Resources:     container.Resources{Devices: devices},
		PortBindings:  portBindings,
		NetworkMode:   networkName,
		Privileged:    options.Privileged,
	}

	created, err := c.api.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err == nil {
		err = c.api.ContainerStart(ctx, created.ID, container.StartOptions{})
	}
	if err != nil {
		c.log.Error("Failed to start app container %s: %s", name, err.Error())
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("start_new_container", true)
	}
	return nil
}

// envList renders an env map as KEY=VALUE strings in sorted key order.
func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// writerOrNil narrows a possibly-nil BuildLog interface to io.Writer.
func writerOrNil(b BuildLog) io.Writer {
	if b == nil {
		return nil
	}
	return b
}

// Stop stops the container; 404/304 are silent like Node.
func (c *Client) Stop(name string) {
	if !c.available {
		return
	}
	if err := c.api.ContainerStop(context.Background(), name, container.StopOptions{}); err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to stop container %s: %s", name, err.Error())
		}
	}
}

// Remove force-removes the container; 404 is silent.
func (c *Client) Remove(name string) {
	if !c.available {
		return
	}
	if err := c.api.ContainerRemove(context.Background(), name, container.RemoveOptions{Force: true}); err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to remove container %s: %s", name, err.Error())
		}
	}
}

// Rename renames a container (used by the Blue-Green switch).
func (c *Client) Rename(oldName, newName string) error {
	return c.api.ContainerRename(context.Background(), oldName, newName)
}

// StreamLogs follows the container's stdout/stderr, demuxing into the two
// writers, until the stream ends or stop() is called. Returns nil when the
// container does not exist (Node returns a nil stream on 404).
func (c *Client) StreamLogs(name string, stdout, stderr io.Writer) (stop func(), err error) {
	if !c.available {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := c.api.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true,
	})
	if err != nil {
		cancel()
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to get logs for %s: %s", name, err.Error())
		}
		return nil, nil
	}
	go func() {
		defer rc.Close()
		stdcopy.StdCopy(stdout, stderr, rc)
	}()
	return func() { cancel(); rc.Close() }, nil
}

// IsRunning reports the container's State.Running (false on any error;
// non-404 errors logged).
func (c *Client) IsRunning(name string) bool {
	if !c.available {
		return false
	}
	data, err := c.api.ContainerInspect(context.Background(), name)
	if err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to check if running %s: %s", name, err.Error())
		}
		return false
	}
	return data.State != nil && data.State.Running
}

// ContainerInfo is one row of List.
type ContainerInfo struct {
	ID      string   `json:"id"`
	Names   []string `json:"names"`
	Image   string   `json:"image"`
	State   string   `json:"state"`
	Status  string   `json:"status"`
	Created int64    `json:"created"`
	Ports   any      `json:"ports"`
}

// List returns all containers (running or not), Node's trimmed projection.
func (c *Client) List() []ContainerInfo {
	if !c.available {
		return nil
	}
	list, err := c.api.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		c.log.Error("Failed to list containers: %s", err.Error())
		return nil
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, ct := range list {
		id := ct.ID
		if len(id) > 12 {
			id = id[:12]
		}
		out = append(out, ContainerInfo{
			ID: id, Names: ct.Names, Image: ct.Image, State: ct.State,
			Status: ct.Status, Created: ct.Created, Ports: ct.Ports,
		})
	}
	return out
}

// GetIP resolves the container's IP: odac-network first, then the first
// network with an address. ("", error) mirrors Node's null.
func (c *Client) GetIP(nameOrID string) (string, error) {
	if !c.available {
		return "", fmt.Errorf("docker not available")
	}
	data, err := c.api.ContainerInspect(context.Background(), nameOrID)
	if err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to get IP for %s: %s", nameOrID, err.Error())
		}
		return "", err
	}
	if data.NetworkSettings == nil {
		return "", fmt.Errorf("no network settings")
	}
	if n := data.NetworkSettings.Networks[networkName]; n != nil && n.IPAddress != "" {
		return n.IPAddress, nil
	}
	// Fallback: first network, in sorted name order for determinism (Node
	// takes JS object order).
	names := make([]string, 0, len(data.NetworkSettings.Networks))
	for name := range data.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if n := data.NetworkSettings.Networks[name]; n != nil && n.IPAddress != "" {
			return n.IPAddress, nil
		}
	}
	return "", fmt.Errorf("no IP address")
}

// GetEnv returns the container's environment as a map (empty on errors).
func (c *Client) GetEnv(name string) map[string]string {
	env := map[string]string{}
	if !c.available {
		return env
	}
	data, err := c.api.ContainerInspect(context.Background(), name)
	if err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to get Env for %s: %s", name, err.Error())
		}
		return env
	}
	if data.Config == nil {
		return env
	}
	for _, e := range data.Config.Env {
		key, val, _ := strings.Cut(e, "=")
		env[key] = val
	}
	return env
}

// GetImageExposedPorts returns the numeric ports of the image's EXPOSE
// metadata (empty on errors).
func (c *Client) GetImageExposedPorts(imageName string) []int {
	if !c.available {
		return nil
	}
	data, _, err := c.api.ImageInspectWithRaw(context.Background(), imageName)
	if err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to inspect image %s: %s", imageName, err.Error())
		}
		return nil
	}
	if data.Config == nil {
		return nil
	}
	var out []int
	keys := make([]string, 0, len(data.Config.ExposedPorts))
	for p := range data.Config.ExposedPorts {
		keys = append(keys, string(p))
	}
	sort.Strings(keys)
	for _, p := range keys {
		numPart, _, _ := strings.Cut(p, "/")
		var n int
		if _, err := fmt.Sscanf(numPart, "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Status is Container.getStatus's shape.
type Status struct {
	Running   bool     `json:"running"`
	Restarts  int      `json:"restarts"`
	StartTime string   `json:"startTime,omitempty"`
	Networks  []string `json:"networks,omitempty"`
}

// GetStatus returns run state, restart count, start time and networks
// (zero Status on errors).
func (c *Client) GetStatus(name string) Status {
	if !c.available {
		return Status{}
	}
	data, err := c.api.ContainerInspect(context.Background(), name)
	if err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to get status for %s: %s", name, err.Error())
		}
		return Status{}
	}

	var networks []string
	if data.NetworkSettings != nil {
		for n := range data.NetworkSettings.Networks {
			networks = append(networks, n)
		}
		sort.Strings(networks)
	}
	if len(networks) == 0 && data.HostConfig != nil && data.HostConfig.NetworkMode != "" {
		networks = []string{string(data.HostConfig.NetworkMode)}
	}

	st := Status{Restarts: data.RestartCount, Networks: networks}
	if data.State != nil {
		st.Running = data.State.Running
		st.StartTime = data.State.StartedAt
	}
	return st
}

// SetNetworksResult is Container.setNetworks's shape.
type SetNetworksResult struct {
	Success  bool
	Message  string
	Networks []string
}

// SetNetworks reconciles the container's networks with the desired list:
// missing target networks are created, extra ones disconnected, new ones
// connected; returns the final list.
func (c *Client) SetNetworks(name string, networks []string) SetNetworksResult {
	if !c.available {
		return SetNetworksResult{Success: false, Message: "Docker is not available"}
	}
	ctx := context.Background()

	fail := func(err error) SetNetworksResult {
		c.log.Error("Failed to set networks for %s: %s", name, err.Error())
		return SetNetworksResult{Success: false, Message: err.Error()}
	}

	data, err := c.api.ContainerInspect(ctx, name)
	if err != nil {
		return fail(err)
	}
	current := map[string]bool{}
	if data.NetworkSettings != nil {
		for n := range data.NetworkSettings.Networks {
			current[n] = true
		}
	}
	desired := map[string]bool{}
	for _, n := range networks {
		desired[n] = true
		c.ensureNetwork(ctx, n)
	}

	currentSorted := make([]string, 0, len(current))
	for n := range current {
		currentSorted = append(currentSorted, n)
	}
	sort.Strings(currentSorted)
	for _, n := range currentSorted {
		if !desired[n] {
			c.log.Log("Disconnecting %s from network %s", name, n)
			if err := c.api.NetworkDisconnect(ctx, n, name, false); err != nil {
				return fail(err)
			}
		}
	}
	for _, n := range networks {
		if !current[n] {
			c.log.Log("Connecting %s to network %s", name, n)
			if err := c.api.NetworkConnect(ctx, n, name, nil); err != nil {
				return fail(err)
			}
		}
	}

	updated, err := c.api.ContainerInspect(ctx, name)
	if err != nil {
		return fail(err)
	}
	var final []string
	if updated.NetworkSettings != nil {
		for n := range updated.NetworkSettings.Networks {
			final = append(final, n)
		}
		sort.Strings(final)
	}
	c.log.Log("Networks updated for %s: [%s]", name, strings.Join(final, ", "))
	return SetNetworksResult{Success: true, Networks: final}
}

// Stats is Container.getStats's shape (JSON keys match Node).
type Stats struct {
	CPUPercent float64 `json:"cpu_percent"`
	Memory     struct {
		Usage   uint64  `json:"usage"`
		Limit   uint64  `json:"limit"`
		Percent float64 `json:"percent"`
	} `json:"memory"`
	Network struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"network"`
	Pids      uint64 `json:"pids"`
	Timestamp int64  `json:"timestamp"`
}

// GetStats returns one-shot CPU/memory/network stats (nil on errors),
// reproducing Node's delta math and 2-decimal rounding.
func (c *Client) GetStats(name string, nowMs int64) *Stats {
	if !c.available {
		return nil
	}
	resp, err := c.api.ContainerStatsOneShot(context.Background(), name)
	if err != nil {
		if !client.IsErrNotFound(err) {
			c.log.Error("Failed to get stats for %s: %s", name, err.Error())
		}
		return nil
	}
	defer resp.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		c.log.Error("Failed to get stats for %s: %s", name, err.Error())
		return nil
	}
	return computeStats(&raw, nowMs)
}

// computeStats ports the stats math so it is testable without a daemon.
func computeStats(raw *container.StatsResponse, nowMs int64) *Stats {
	s := &Stats{Timestamp: nowMs}

	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(raw.CPUStats.SystemUsage) - float64(raw.PreCPUStats.SystemUsage)
	onlineCPUs := float64(raw.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(raw.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}
	if systemDelta > 0 && cpuDelta > 0 {
		s.CPUPercent = round2(cpuDelta / systemDelta * onlineCPUs * 100)
	}

	s.Memory.Usage = raw.MemoryStats.Usage
	s.Memory.Limit = raw.MemoryStats.Limit
	if s.Memory.Limit > 0 {
		s.Memory.Percent = round2(float64(s.Memory.Usage) / float64(s.Memory.Limit) * 100)
	}

	for _, n := range raw.Networks {
		s.Network.RxBytes += n.RxBytes
		s.Network.TxBytes += n.TxBytes
	}
	s.Pids = raw.PidsStats.Current
	return s
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// jsString renders a decoded-JSON scalar the way JS string interpolation
// would (numbers without a trailing ".0").
func jsString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case int:
		return fmt.Sprintf("%d", x)
	case nil:
		return ""
	}
	return fmt.Sprintf("%v", v)
}
