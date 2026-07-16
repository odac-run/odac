package docker

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

func newTestClient(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	return New(f, Options{LogsRoot: t.TempDir()})
}

func TestAvailability(t *testing.T) {
	f := newFakeAPI()
	if !newTestClient(t, f).Available() {
		t.Error("ping ok should mean available")
	}

	f2 := newFakeAPI()
	f2.pingErr = notFoundErr{"down"}
	c := newTestClient(t, f2)
	if c.Available() {
		t.Error("ping failure should mean unavailable")
	}
	// Unavailable clients no-op instead of calling the API (Node: return false).
	if started, err := c.RunApp("x", RunOptions{Image: "img"}, nil, nil); started || err != nil {
		t.Errorf("RunApp while unavailable should be a (false, nil) no-op, got (%v, %v)", started, err)
	}
	if len(f2.created) != 0 {
		t.Error("no container should be created while unavailable")
	}
}

func TestResolveHostPath(t *testing.T) {
	c := newTestClient(t, newFakeAPI())
	if got := c.ResolveHostPath("/app/x"); got != "/app/x" {
		t.Errorf("no host root: %q", got)
	}
	c.hostRoot = "/var/odac"
	if got := c.ResolveHostPath("/app/apps/site"); got != "/var/odac/apps/site" {
		t.Errorf("host root mapping: %q", got)
	}
	if got := c.ResolveHostPath("/other/path"); got != "/other/path" {
		t.Errorf("non-/app path must pass through: %q", got)
	}
}

func TestRunAppPortAssembly(t *testing.T) {
	f := newFakeAPI()
	f.images["img"] = image.InspectResponse{}
	c := newTestClient(t, f)

	started, err := c.RunApp("myapp", RunOptions{
		Image: "img",
		Ports: []map[string]any{
			{"host": "proxy", "container": 3000.0},                 // skipped: proxy-routed
			{"host": 8080.0, "container": 80.0},                    // loopback publish
			{"host": 9090.0, "container": 80.0, "public": true},    // public publish, same container port
			{"host": "7000", "container": 7000.0, "public": false}, // string host port
		},
		Volumes: []Mount{{Host: "/data", Container: "/data"}},
		Devices: []Device{{Host: "/dev/ttyACM0"}},
		Env:     map[string]string{"B": "2", "A": "1"},
		Cmd:     []string{"run", "it"},
		User:    "root",
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Error("successful RunApp must report started=true")
	}

	if len(f.created) != 1 {
		t.Fatalf("created %d containers", len(f.created))
	}
	call := f.created[0]
	if call.Name != "myapp" {
		t.Errorf("name = %q", call.Name)
	}

	hc := call.HostConfig
	wantBindings := nat.PortMap{
		"80/tcp": {
			{HostIP: "127.0.0.1", HostPort: "8080"},
			{HostIP: "", HostPort: "9090"},
		},
		"7000/tcp": {{HostIP: "127.0.0.1", HostPort: "7000"}},
	}
	if !reflect.DeepEqual(hc.PortBindings, wantBindings) {
		t.Errorf("port bindings = %#v", hc.PortBindings)
	}
	if _, ok := call.Config.ExposedPorts["3000/tcp"]; ok {
		t.Error("proxy-routed port must not be exposed")
	}
	if _, ok := call.Config.ExposedPorts["80/tcp"]; !ok {
		t.Error("published port must be exposed")
	}

	if hc.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("restart policy = %q", hc.RestartPolicy.Name)
	}
	if string(hc.NetworkMode) != "odac-network" {
		t.Errorf("network mode = %q", hc.NetworkMode)
	}
	if len(hc.Binds) != 1 || hc.Binds[0] != "/data:/data" {
		t.Errorf("binds = %v", hc.Binds)
	}
	if len(hc.Resources.Devices) != 1 || hc.Resources.Devices[0].PathInContainer != "/dev/ttyACM0" {
		t.Errorf("devices = %v", hc.Resources.Devices)
	}
	if !reflect.DeepEqual(call.Config.Env, []string{"A=1", "B=2"}) {
		t.Errorf("env = %v (want sorted)", call.Config.Env)
	}
	if call.Config.User != "root" || hc.Privileged {
		t.Errorf("user/privileged = %q/%v", call.Config.User, hc.Privileged)
	}
	if !reflect.DeepEqual([]string(call.Config.Cmd), []string{"run", "it"}) {
		t.Errorf("cmd = %v", call.Config.Cmd)
	}

	// Force-replace: the old name is removed before create, network ensured.
	if len(f.removed) == 0 || f.removed[0] != "myapp" {
		t.Errorf("existing container not removed first: %v", f.removed)
	}
	found := false
	for _, n := range f.networks {
		if n == "odac-network" {
			found = true
		}
	}
	if !found {
		t.Error("odac-network not ensured")
	}
	if len(f.started) != 1 {
		t.Errorf("started = %v", f.started)
	}
}

func TestRunAppPrivileged(t *testing.T) {
	f := newFakeAPI()
	f.images["img"] = image.InspectResponse{}
	c := newTestClient(t, f)
	if _, err := c.RunApp("p", RunOptions{Image: "img", Privileged: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !f.created[0].HostConfig.Privileged {
		t.Error("privileged flag lost")
	}
}

// TestRunAppCancellation pins the three isCancelled checkpoints from dev
// 8a399e6 + a4c6285 (no jest coverage there — this is the spec): before the
// image pull, after it, and between create and start, where the created
// container is force-removed.
func TestRunAppCancellation(t *testing.T) {
	runUntil := func(cancelAt int) (*fakeAPI, bool, error) {
		f := newFakeAPI()
		f.images["img"] = image.InspectResponse{}
		c := newTestClient(t, f)
		checks := 0
		started, err := c.RunApp("app", RunOptions{Image: "img"}, nil, func() bool {
			checks++
			return checks >= cancelAt
		})
		return f, started, err
	}

	t.Run("before image pull", func(t *testing.T) {
		f, started, err := runUntil(1)
		if started || err != nil {
			t.Fatalf("got (%v, %v), want (false, nil)", started, err)
		}
		if len(f.created) != 0 || len(f.started) != 0 {
			t.Errorf("created/started after cancel: %v %v", f.created, f.started)
		}
	})

	t.Run("after image pull", func(t *testing.T) {
		f, started, err := runUntil(2)
		if started || err != nil {
			t.Fatalf("got (%v, %v), want (false, nil)", started, err)
		}
		if len(f.created) != 0 || len(f.started) != 0 {
			t.Errorf("created/started after cancel: %v %v", f.created, f.started)
		}
	})

	t.Run("between create and start", func(t *testing.T) {
		f, started, err := runUntil(3)
		if started || err != nil {
			t.Fatalf("got (%v, %v), want (false, nil)", started, err)
		}
		if len(f.created) != 1 {
			t.Fatalf("created = %v", f.created)
		}
		if len(f.started) != 0 {
			t.Errorf("cancelled container was started: %v", f.started)
		}
		// The orphan is force-removed by its fresh ID (the name-keyed remove
		// at the top of RunApp is the force-replace, not this).
		id := f.created[0].ID
		removedByID := false
		for _, rm := range f.removed {
			if rm == id {
				removedByID = true
			}
		}
		if !removedByID {
			t.Errorf("created container %s not removed: %v", id, f.removed)
		}
	})

	t.Run("never cancelled runs to completion", func(t *testing.T) {
		f, started, err := runUntil(99)
		if !started || err != nil {
			t.Fatalf("got (%v, %v), want (true, nil)", started, err)
		}
		if len(f.started) != 1 {
			t.Errorf("started = %v", f.started)
		}
	})
}

func TestEnsureImagePullsOnlyWhenMissing(t *testing.T) {
	f := newFakeAPI()
	f.images["have:latest"] = image.InspectResponse{}
	c := newTestClient(t, f)

	var log bytes.Buffer
	if err := c.EnsureImage("have:latest", &log); err != nil {
		t.Fatal(err)
	}
	if len(f.pulled) != 0 {
		t.Error("existing image must not be pulled")
	}
	if !strings.Contains(log.String(), "already exists locally") {
		t.Errorf("log = %q", log.String())
	}

	log.Reset()
	f.pullBodies["need:1"] = `{"status":"Pulling fs layer","progress":"[==> ]"}` + "\n" +
		`{"stream":"raw line\n"}` + "\n"
	if err := c.EnsureImage("need:1", &log); err != nil {
		t.Fatal(err)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "need:1" {
		t.Errorf("pulled = %v", f.pulled)
	}
	out := log.String()
	if !strings.Contains(out, "Pulling fs layer [==> ]\n") {
		t.Errorf("status+progress rendering missing: %q", out)
	}
	if !strings.Contains(out, "raw line\n") {
		t.Errorf("stream passthrough missing: %q", out)
	}
	if !strings.Contains(out, "pulled successfully") {
		t.Errorf("completion line missing: %q", out)
	}
}

func TestEnsureImagePullErrorEvent(t *testing.T) {
	f := newFakeAPI()
	f.pullBodies["bad:1"] = `{"error":"manifest unknown"}` + "\n"
	c := newTestClient(t, f)
	if err := c.EnsureImage("bad:1", nil); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("err = %v", err)
	}
}

func TestCloneRepoCommandAssembly(t *testing.T) {
	f := newFakeAPI()
	f.images[gitImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	if err := c.CloneRepo("https://github.com/u/r.git", "main", "/apps/r", "sekret", nil); err != nil {
		t.Fatal(err)
	}
	call := f.created[0]
	if got := call.Config.Cmd[0]; got != `git clone --depth 1 --branch "$GIT_BRANCH" "$GIT_REMOTE_URL" .` {
		t.Errorf("cmd = %q", got)
	}
	wantEnv := []string{"GIT_REMOTE_URL=https://oauth2:sekret@github.com/u/r.git", "GIT_BRANCH=main"}
	if !reflect.DeepEqual(call.Config.Env, wantEnv) {
		t.Errorf("env = %v", call.Config.Env)
	}
	if !reflect.DeepEqual([]string(call.Config.Entrypoint), []string{"sh", "-c"}) {
		t.Errorf("entrypoint = %v", call.Config.Entrypoint)
	}
	if call.Config.WorkingDir != "/git" || call.HostConfig.Binds[0] != "/apps/r:/git" {
		t.Errorf("workdir/binds = %q/%v", call.Config.WorkingDir, call.HostConfig.Binds)
	}
	if call.HostConfig.Privileged {
		t.Error("git sandbox must be unprivileged")
	}
	// Ephemeral container force-removed afterwards.
	if f.removed[len(f.removed)-1] != call.ID {
		t.Errorf("git container not removed: %v", f.removed)
	}
}

func TestCloneRepoNoBranchNoToken(t *testing.T) {
	f := newFakeAPI()
	f.images[gitImage] = image.InspectResponse{}
	c := newTestClient(t, f)
	if err := c.CloneRepo("https://github.com/u/r.git", "", "/apps/r", "", nil); err != nil {
		t.Fatal(err)
	}
	call := f.created[0]
	if got := call.Config.Cmd[0]; got != `git clone --depth 1 "$GIT_REMOTE_URL" .` {
		t.Errorf("cmd = %q", got)
	}
	if !reflect.DeepEqual(call.Config.Env, []string{"GIT_REMOTE_URL=https://github.com/u/r.git"}) {
		t.Errorf("env = %v", call.Config.Env)
	}
}

func TestCloneRepoFailureCode(t *testing.T) {
	f := newFakeAPI()
	f.images[gitImage] = image.InspectResponse{}
	c := newTestClient(t, f)
	// First created container will be ctr1.
	f.waitCodes["ctr1"] = 128
	err := c.CloneRepo("https://x/y.git", "", "/apps/y", "", nil)
	if err == nil || err.Error() != "Git clone failed with exit code 128." {
		t.Errorf("err = %v", err)
	}
}

func TestFetchRepoCommandAssembly(t *testing.T) {
	f := newFakeAPI()
	f.images[gitImage] = image.InspectResponse{}
	c := newTestClient(t, f)

	if err := c.FetchRepo("https://github.com/u/r.git", "dev", "/apps/r", "tok", "abc123", nil); err != nil {
		t.Fatal(err)
	}
	call := f.created[0]
	want := `git remote set-url origin "$GIT_REMOTE_URL" && git fetch --depth 1 origin "$GIT_COMMIT_SHA" && git reset --hard "$GIT_COMMIT_SHA" && git remote set-url origin "$GIT_ORIGINAL_URL"`
	if got := call.Config.Cmd[0]; got != want {
		t.Errorf("cmd = %q", got)
	}
	wantEnv := []string{
		"GIT_BRANCH=dev",
		"GIT_COMMIT_SHA=abc123",
		"GIT_REMOTE_URL=https://oauth2:tok@github.com/u/r.git",
		"GIT_ORIGINAL_URL=https://github.com/u/r.git",
	}
	if !reflect.DeepEqual(call.Config.Env, wantEnv) {
		t.Errorf("env = %v", call.Config.Env)
	}
}

func TestMaskingWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &maskingWriter{w: &buf, secret: "tok123"}
	w.Write([]byte("url https://oauth2:tok123@host and tok123 again\n"))
	if strings.Contains(buf.String(), "tok123") {
		t.Errorf("token leaked: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "oauth2:*****@host") {
		t.Errorf("mask missing: %q", buf.String())
	}
}

func TestExecInContainer(t *testing.T) {
	f := newFakeAPI()
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: true}},
	}
	f.execOutputs["echo hi"] = "hi\n"
	c := newTestClient(t, f)

	out, err := c.ExecInContainer("web", "echo hi")
	if err != nil || out != "hi\n" {
		t.Errorf("out/err = %q/%v", out, err)
	}

	f.execCodes["false"] = 1
	if _, err := c.ExecInContainer("web", "false"); err == nil || !strings.Contains(err.Error(), "Exit Code 1") {
		t.Errorf("err = %v", err)
	}

	if _, err := c.ExecInContainer("gone", "echo"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v", err)
	}
}

func TestParseProcNetTCP(t *testing.T) {
	proc := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid
   0: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000
   2: 00000000:01BB 00000000:0000 01 00000000:00000000 00:00000000 00000000  1000
   3: 00000000:F230 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000
`
	got := parseProcNetTCP(proc)
	// 0BB8=3000 listed; 1F90=8080 loopback skipped; 01BB=443 state 01 skipped;
	// F230=62000 >= 60000 skipped.
	if !reflect.DeepEqual(got, []int{3000}) {
		t.Errorf("ports = %v", got)
	}
}

func TestGetListeningPortsMergesTables(t *testing.T) {
	f := newFakeAPI()
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: true}},
	}
	f.execOutputs["cat /proc/net/tcp"] = "header\n 0: 00000000:0BB8 00000000:0000 0A x\n"
	f.execOutputs["cat /proc/net/tcp6"] = "header\n 0: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A x\n 1: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A x\n"
	c := newTestClient(t, f)

	got := c.GetListeningPorts("web")
	if !reflect.DeepEqual(got, []int{3000, 80}) {
		t.Errorf("ports = %v", got)
	}
}

func TestComputeStats(t *testing.T) {
	raw := &container.StatsResponse{}
	raw.CPUStats.CPUUsage.TotalUsage = 200
	raw.PreCPUStats.CPUUsage.TotalUsage = 100
	raw.CPUStats.SystemUsage = 2000
	raw.PreCPUStats.SystemUsage = 1000
	raw.CPUStats.OnlineCPUs = 4
	raw.MemoryStats.Usage = 512
	raw.MemoryStats.Limit = 2048
	raw.Networks = map[string]container.NetworkStats{
		"eth0": {RxBytes: 10, TxBytes: 20},
		"eth1": {RxBytes: 1, TxBytes: 2},
	}
	raw.PidsStats.Current = 7

	s := computeStats(raw, 12345)
	if s.CPUPercent != 40 { // 100/1000 * 4 * 100
		t.Errorf("cpu = %v", s.CPUPercent)
	}
	if s.Memory.Percent != 25 {
		t.Errorf("mem%% = %v", s.Memory.Percent)
	}
	if s.Network.RxBytes != 11 || s.Network.TxBytes != 22 {
		t.Errorf("net = %+v", s.Network)
	}
	if s.Pids != 7 || s.Timestamp != 12345 {
		t.Errorf("pids/ts = %v/%v", s.Pids, s.Timestamp)
	}
}

func TestGetStatsDecodesBody(t *testing.T) {
	f := newFakeAPI()
	body, _ := json.Marshal(map[string]any{
		"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 200}, "system_cpu_usage": 2000, "online_cpus": 1},
		"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 100}, "system_cpu_usage": 1000},
		"memory_stats": map[string]any{"usage": 100, "limit": 400},
		"pids_stats":   map[string]any{"current": 3},
	})
	f.statsBody = string(body)
	c := newTestClient(t, f)
	s := c.GetStats("web", 1)
	if s == nil || s.CPUPercent != 10 || s.Memory.Percent != 25 || s.Pids != 3 {
		t.Errorf("stats = %+v", s)
	}
}

func TestGetIPPrefersOdacNetwork(t *testing.T) {
	f := newFakeAPI()
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"bridge":       {IPAddress: "172.17.0.2"},
				"odac-network": {IPAddress: "172.20.0.5"},
			},
		},
	}
	c := newTestClient(t, f)
	if ip, err := c.GetIP("web"); err != nil || ip != "172.20.0.5" {
		t.Errorf("ip = %q err = %v", ip, err)
	}

	f.inspects["other"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{"bridge": {IPAddress: "172.17.0.9"}},
		},
	}
	if ip, _ := c.GetIP("other"); ip != "172.17.0.9" {
		t.Errorf("fallback ip = %q", ip)
	}
}

func TestGetStatusAndEnvAndExposedPorts(t *testing.T) {
	f := newFakeAPI()
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State:        &container.State{Running: true, StartedAt: "2026-07-11T00:00:00Z"},
			RestartCount: 3,
			HostConfig:   &container.HostConfig{NetworkMode: "odac-network"},
		},
		Config: &container.Config{Env: []string{"A=1", "B=x=y"}},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{"odac-network": {}},
		},
	}
	c := newTestClient(t, f)

	st := c.GetStatus("web")
	if !st.Running || st.Restarts != 3 || st.StartTime != "2026-07-11T00:00:00Z" || len(st.Networks) != 1 {
		t.Errorf("status = %+v", st)
	}
	if st := c.GetStatus("missing"); st.Running || st.Restarts != 0 {
		t.Errorf("missing status = %+v", st)
	}

	env := c.GetEnv("web")
	if env["A"] != "1" || env["B"] != "x=y" {
		t.Errorf("env = %v (multi-= value must survive)", env)
	}
}

// exposedConfig builds an image config declaring the given EXPOSE ports.
func exposedConfig(ports ...string) *dockerspec.DockerOCIImageConfig {
	cfg := &dockerspec.DockerOCIImageConfig{}
	cfg.ExposedPorts = map[string]struct{}{}
	for _, p := range ports {
		cfg.ExposedPorts[p] = struct{}{}
	}
	return cfg
}

func TestGetImageExposedPorts(t *testing.T) {
	f := newFakeAPI()
	c := newTestClient(t, f)
	f.images["img"] = image.InspectResponse{
		Config: exposedConfig("3000/tcp", "53/udp", "bogus/tcp"),
	}
	got := c.GetImageExposedPorts("img")
	if !reflect.DeepEqual(got, []int{3000, 53}) {
		t.Errorf("exposed = %v", got)
	}
	if got := c.GetImageExposedPorts("none"); len(got) != 0 {
		t.Errorf("missing image should be empty, got %v", got)
	}
}

func TestSetNetworks(t *testing.T) {
	f := newFakeAPI()
	f.networks = []string{"keep", "drop"}
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{"keep": {}, "drop": {}},
		},
	}
	c := newTestClient(t, f)

	res := c.SetNetworks("web", []string{"keep", "add"})
	if !res.Success {
		t.Fatalf("res = %+v", res)
	}
	if len(f.netDisconnects) != 1 || f.netDisconnects[0][0] != "drop" {
		t.Errorf("disconnects = %v", f.netDisconnects)
	}
	if len(f.netConnects) != 1 || f.netConnects[0][0] != "add" {
		t.Errorf("connects = %v", f.netConnects)
	}
	// "add" network was created on demand.
	found := false
	for _, n := range f.networks {
		if n == "add" {
			found = true
		}
	}
	if !found {
		t.Error("target network not ensured")
	}
}

func TestListProjection(t *testing.T) {
	c := newTestClient(t, newFakeAPI())
	list := c.List()
	if len(list) != 1 || list[0].ID != "abcdef012345" || list[0].Names[0] != "/one" {
		t.Errorf("list = %+v", list)
	}
}

func TestIsRunning(t *testing.T) {
	f := newFakeAPI()
	f.inspects["up"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: true}},
	}
	c := newTestClient(t, f)
	if !c.IsRunning("up") {
		t.Error("up should be running")
	}
	if c.IsRunning("down") {
		t.Error("missing container should not be running")
	}
}
