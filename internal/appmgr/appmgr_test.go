package appmgr

// Test harness: fakes for the Docker/Api/Proxy/Hub/Domains seams plus a
// Manager builder. The scenarios are ported from test/server/App.test.js,
// deviceAdd.test.js and deviceDelete.test.js (the Go spec per PLAN 3.4);
// Deploy.js had ~14% Node coverage, so deploy_test.go pins its behavior from
// the line-by-line read.

import (
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"odac/internal/applog"
	"odac/internal/config"
	"odac/internal/docker"
	"odac/internal/lang"
)

func init() { lang.SetLocale("en-US") }

// ---- fake Docker ----

type runCall struct {
	name        string
	options     docker.RunOptions
	isCancelled func() bool
}

type fakeDocker struct {
	mu        sync.Mutex
	available bool

	listening map[string][]int // by container name; nil key "" = default
	ips       map[string]string
	exposed   map[string][]int // by image
	running   map[string]bool
	status    map[string]docker.Status
	statDirs  map[string]bool // key: name\x00containerPath → isDir; absent = unknown

	runCalls      []runCall
	runErr        func(name string) error
	runBlock      chan struct{}     // when set, RunApp waits for it
	runHook       func(name string) // fires inside RunApp before its cancel check
	stopped       []string
	stopHook      func(name string) // fires inside Stop, before recording
	removed       []string
	removedImages []string
	prunedImages  bool
	renames       [][2]string
	renameErr     error

	fetchHook func() // fires inside FetchRepo
	buildHook func() // fires inside Build

	cloneCalls [][2]string // url, branch
	cloneErr   error
	fetchCalls [][2]string
	buildCalls []string // image names
	buildErr   error

	containers []docker.ContainerInfo

	networksResult docker.SetNetworksResult

	registered   []string
	unregistered []string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		available: true,
		listening: map[string][]int{"": {3000}}, // default: pass readiness
		ips:       map[string]string{"": "10.0.0.5"},
		exposed:   map[string][]int{},
		running:   map[string]bool{},
		status:    map[string]docker.Status{},
	}
}

func (f *fakeDocker) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

func (f *fakeDocker) RunApp(name string, options docker.RunOptions, _ docker.BuildLog, isCancelled func() bool) (bool, error) {
	f.mu.Lock()
	f.runCalls = append(f.runCalls, runCall{name: name, options: options, isCancelled: isCancelled})
	block := f.runBlock
	runErr := f.runErr
	runHook := f.runHook
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if runHook != nil {
		runHook(name)
	}
	if runErr != nil {
		return false, runErr(name)
	}
	// The real RunApp consults isCancelled before starting the container.
	if isCancelled != nil && isCancelled() {
		return false, nil
	}
	f.mu.Lock()
	f.running[name] = true
	f.mu.Unlock()
	return true, nil
}

func (f *fakeDocker) runCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runCalls)
}

func (f *fakeDocker) runCallAt(i int) runCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCalls[i]
}

func (f *fakeDocker) Stop(name string) {
	f.mu.Lock()
	hook := f.stopHook
	f.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	delete(f.running, name)
}

func (f *fakeDocker) Remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
}

func (f *fakeDocker) RemoveImage(imageName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedImages = append(f.removedImages, imageName)
}

func (f *fakeDocker) PruneDanglingImages() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunedImages = true
}

func (f *fakeDocker) Rename(oldName, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renameErr != nil {
		return f.renameErr
	}
	f.renames = append(f.renames, [2]string{oldName, newName})
	return nil
}

func (f *fakeDocker) StreamLogs(string, io.Writer, io.Writer) (func(), error) {
	return func() {}, nil
}

func (f *fakeDocker) IsRunning(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[name]
}

func (f *fakeDocker) List() []docker.ContainerInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.ContainerInfo{}, f.containers...)
}

func (f *fakeDocker) GetIP(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ip, ok := f.ips[name]; ok {
		return ip, nil
	}
	if ip, ok := f.ips[""]; ok {
		return ip, nil
	}
	return "", errors.New("no ip")
}

func (f *fakeDocker) GetStatus(name string) docker.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.status[name]; ok {
		return st
	}
	return docker.Status{Running: f.running[name]}
}

func (f *fakeDocker) GetListeningPorts(name string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.listening[name]; ok {
		return l
	}
	return f.listening[""]
}

// setListening scopes the listening ports to one container name; every
// other name gets nothing (the Node suite's name-scoped mock, 767bdbb).
func (f *fakeDocker) setListening(name string, ports []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listening = map[string][]int{name: ports}
}

// dropIPs makes GetIP fail for every container, the way the real client
// behaves for a host-networked one (it has no address of its own).
func (f *fakeDocker) dropIPs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips = map[string]string{}
}

func (f *fakeDocker) GetImageExposedPorts(image string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exposed[image]
}

func (f *fakeDocker) SetNetworks(string, []string) docker.SetNetworksResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networksResult
}

func (f *fakeDocker) EnsureImage(string, io.Writer) error { return nil }

func (f *fakeDocker) StatPathIsDir(name, containerPath string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statDirs == nil {
		return false, false
	}
	isDir, ok := f.statDirs[name+"\x00"+containerPath]
	return isDir, ok
}

func (f *fakeDocker) CloneRepo(url, branch, _, _ string, _ docker.BuildLog) error {
	f.mu.Lock()
	f.cloneCalls = append(f.cloneCalls, [2]string{url, branch})
	err := f.cloneErr
	f.mu.Unlock()
	return err
}

func (f *fakeDocker) FetchRepo(url, branch, _, _, _ string, _ docker.BuildLog) error {
	f.mu.Lock()
	f.fetchCalls = append(f.fetchCalls, [2]string{url, branch})
	hook := f.fetchHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeDocker) Build(_, imageName, _ string, _ docker.BuildLog) error {
	f.mu.Lock()
	f.buildCalls = append(f.buildCalls, imageName)
	hook := f.buildHook
	err := f.buildErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (f *fakeDocker) RegisterBuildLogger(appName string, _ *applog.Logger) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, appName)
}

func (f *fakeDocker) UnregisterBuildLogger(appName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered = append(f.unregistered, appName)
}

func (f *fakeDocker) ResolveHostPath(localPath string) string { return localPath }

// ---- other fakes ----

type fakeAPI struct {
	mu    sync.Mutex
	calls [][2]any // name, permissions
}

func (f *fakeAPI) GenerateAppToken(appName string, permissions any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]any{appName, permissions})
	return "mock-app-token"
}

func (f *fakeAPI) HostSocketDir() string { return "/tmp/odac-socket" }

type fakeProxy struct {
	mu     sync.Mutex
	syncs  int
	purges []any
}

func (f *fakeProxy) SyncConfig() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
}

func (f *fakeProxy) PurgeCacheForApp(appID any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges = append(f.purges, appID)
}

type fakeHub struct {
	mu       sync.Mutex
	triggers []string
	recipe   map[string]any
	getErr   error
}

func (f *fakeHub) Trigger(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers = append(f.triggers, event)
}

func (f *fakeHub) GetApp(string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recipe, f.getErr
}

// fakeGPUHost answers the create-time GPU pre-flight. Default: this host can
// do anything, so tests that do not care are unaffected.
type fakeGPUHost struct {
	mu       sync.Mutex
	runtimes map[string]bool // nil = allow everything
	asked    []string
}

func (f *fakeGPUHost) CanPassthrough(runtime string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, runtime)
	if f.runtimes == nil {
		return true
	}
	return f.runtimes[runtime]
}

func (f *fakeGPUHost) allow(runtimes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runtimes = map[string]bool{}
	for _, r := range runtimes {
		f.runtimes[r] = true
	}
}

type fakeDomains struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeDomains) DeleteByApp(appName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, appName)
	return nil
}

// ---- harness ----

type fixture struct {
	m       *Manager
	cfg     *config.Store
	dock    *fakeDocker
	api     *fakeAPI
	proxy   *fakeProxy
	hub     *fakeHub
	domains *fakeDomains
	gpuHost *fakeGPUHost

	probeMu   sync.Mutex
	httpPorts map[int]bool // ports that answer the HTTP probe
}

// setHTTPPorts names the container ports that answer HTTP probes (the jest
// suite's http.httpPorts set).
func (fx *fixture) setHTTPPorts(ports ...int) {
	fx.probeMu.Lock()
	defer fx.probeMu.Unlock()
	fx.httpPorts = map[int]bool{}
	for _, p := range ports {
		fx.httpPorts[p] = true
	}
}

// newFixture builds an initialized Manager over a temp config store. apps is
// the initial config.apps value (nil = absent, like an empty install).
func newFixture(t *testing.T, apps any) *fixture {
	t.Helper()
	base := t.TempDir()
	cfg, err := config.Open(base)
	if err != nil {
		t.Fatalf("config.Open: %v", err)
	}
	cfg.Mutate(func() {
		if apps != nil {
			cfg.Set("apps", apps)
		}
		cfg.Set("app", map[string]any{"path": filepath.Join(base, "apps")})
	})

	fx := &fixture{
		cfg:       cfg,
		dock:      newFakeDocker(),
		api:       &fakeAPI{},
		proxy:     &fakeProxy{},
		hub:       &fakeHub{},
		domains:   &fakeDomains{},
		gpuHost:   &fakeGPUHost{},
		httpPorts: map[int]bool{},
	}
	fx.m = New(cfg, filepath.Join(base, "logs"), Deps{
		Docker:  fx.dock,
		Api:     fx.api,
		Proxy:   fx.proxy,
		Hub:     fx.hub,
		Domains: fx.domains,
		GPUHost: fx.gpuHost,
	})
	// Collapse every wait: the poll/readiness budgets stay attempt-counted.
	fx.m.sleep = func(time.Duration) {}
	fx.m.httpProbe = func(_ string, port int, _ time.Duration, _ string) bool {
		fx.probeMu.Lock()
		defer fx.probeMu.Unlock()
		return fx.httpPorts[port]
	}
	fx.m.Init()
	return fx
}

// app reads config.apps[i] as persisted (the post-saveApps view).
func (fx *fixture) app(i int) map[string]any {
	var out map[string]any
	fx.cfg.View(func() {
		apps, _ := fx.cfg.Get("apps").([]any)
		if i < len(apps) {
			out, _ = apps[i].(map[string]any)
		}
	})
	return out
}

func (fx *fixture) appCount() int {
	n := 0
	fx.cfg.View(func() {
		apps, _ := fx.cfg.Get("apps").([]any)
		n = len(apps)
	})
	return n
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitIdle drains every tracked background goroutine (check dispatches, port
// pollers, HTTP scans).
func (fx *fixture) waitIdle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fx.m.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out draining background goroutines")
	}
}

// checkAndSettle runs one watchdog pulse and waits for the runs it spawned.
func (fx *fixture) checkAndSettle(t *testing.T) {
	t.Helper()
	fx.m.Check()
	fx.waitIdle(t)
}

// ---- configuration handling (App.test.js "configuration handling") ----

func TestConfigHandling(t *testing.T) {
	cases := []struct {
		name string
		apps any
	}{
		{"undefined apps", nil},
		{"null apps", nil},
		{"non-array apps", map[string]any{"some": "object"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, tc.apps)
			fx.m.Check() // must not panic
			r := fx.m.List(true)
			if !r.Status {
				t.Fatalf("list failed: %v", r.Message)
			}
			if data, _ := r.Data.([]any); len(data) != 0 {
				t.Fatalf("expected empty list, got %v", r.Data)
			}
		})
	}

	t.Run("valid apps array", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{"id": float64(1), "name": "test-app", "active": true, "type": "container"}})
		r := fx.m.List(true)
		data, _ := r.Data.([]any)
		if len(data) != 1 {
			t.Fatalf("expected 1 app, got %d", len(data))
		}
		if app, _ := data[0].(map[string]any); app["name"] != "test-app" {
			t.Fatalf("wrong app: %v", data[0])
		}
	})
}

// ---- concurrency control ----

func TestCheckPreventsConcurrentRuns(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(999), "name": "concurrent-app", "active": true,
		"type": "container", "status": "running",
	}})

	block := make(chan struct{})
	fx.dock.mu.Lock()
	fx.dock.runBlock = block
	fx.dock.mu.Unlock()

	fx.m.Check()
	waitFor(t, "first run to start", func() bool { return fx.dock.runCallCount() == 1 })
	fx.m.Check()
	fx.m.Check()

	if got := fx.dock.runCallCount(); got != 1 {
		t.Fatalf("expected 1 runApp call, got %d", got)
	}

	close(block)
	fx.waitIdle(t)
}

func TestCreateFromGitConcurrencyGuard(t *testing.T) {
	fx := newFixture(t, []any{})

	block := make(chan struct{})
	release := sync.OnceFunc(func() { close(block) })
	defer release()

	// Block the first create inside CloneRepo.
	started := make(chan struct{})
	fx.m.deps.Docker = &blockingCloneDocker{fakeDocker: fx.dock, started: started, block: block}

	cfgPayload := map[string]any{"type": "git", "url": "https://github.com/test/repo.git", "name": "concurrent-git-app"}

	done := make(chan *string, 1)
	go func() {
		r := fx.m.Create(cfgPayload)
		msg := jsString(r.Message)
		done <- &msg
	}()
	<-started

	r2 := fx.m.Create(cfgPayload)
	if r2.Status {
		t.Fatalf("second create should fail, got success")
	}
	if msg := jsString(r2.Message); msg != "App concurrent-git-app is already being created" {
		t.Fatalf("unexpected message: %q", msg)
	}

	release()
	<-done
	fx.waitIdle(t)
}

type blockingCloneDocker struct {
	*fakeDocker
	started chan struct{}
	block   chan struct{}
	once    sync.Once
}

func (b *blockingCloneDocker) CloneRepo(url, branch, dir, token string, l docker.BuildLog) error {
	b.once.Do(func() { close(b.started) })
	<-b.block
	return b.fakeDocker.CloneRepo(url, branch, dir, token, l)
}

// ---- api permission handling ----

func TestAPITokenAndSocketInjection(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(101), "name": "api-aware-app", "active": true,
		"type": "container", "image": "test:latest", "api": true,
	}})

	fx.checkAndSettle(t)

	if fx.dock.runCallCount() == 0 {
		t.Fatal("runApp not called")
	}
	opts := fx.dock.runCallAt(0).options
	if opts.Env["ODAC_API_KEY"] != "mock-app-token" {
		t.Fatalf("ODAC_API_KEY = %q", opts.Env["ODAC_API_KEY"])
	}
	if opts.Env["ODAC_API_SOCKET"] != "/odac/api.sock" {
		t.Fatalf("ODAC_API_SOCKET = %q", opts.Env["ODAC_API_SOCKET"])
	}
	foundMount := false
	for _, v := range opts.Volumes {
		if v.Container == "/odac:ro" && v.Host == "/tmp/odac-socket" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Fatalf("socket mount missing: %v", opts.Volumes)
	}
}

// ---- Blue-Green API token identity ----

func TestAppIdentityForTokens(t *testing.T) {
	t.Run("uses _appIdentity when present", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(201), "name": "zdd-app-green-build_12345", "_appIdentity": "zdd-app",
			"active": true, "type": "container", "image": "test:latest", "api": []any{"app.list"},
		}})

		fx.checkAndSettle(t)

		fx.api.mu.Lock()
		defer fx.api.mu.Unlock()
		if len(fx.api.calls) == 0 {
			t.Fatal("generateAppToken not called")
		}
		if fx.api.calls[0][0] != "zdd-app" {
			t.Fatalf("token app name = %v", fx.api.calls[0][0])
		}
	})

	t.Run("uses app.name when absent", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(301), "name": "normal-app", "active": true,
			"type": "container", "image": "test:latest", "api": true,
		}})

		fx.checkAndSettle(t)

		fx.api.mu.Lock()
		defer fx.api.mu.Unlock()
		if len(fx.api.calls) == 0 || fx.api.calls[0][0] != "normal-app" {
			t.Fatalf("token calls = %v", fx.api.calls)
		}
	})

	t.Run("strips _appIdentity from saved config", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(401), "name": "saved-app", "_appIdentity": "should-be-removed",
			"active": true, "type": "container", "image": "test:latest",
		}})

		fx.checkAndSettle(t)

		if _, present := fx.app(0)["_appIdentity"]; present {
			t.Fatalf("_appIdentity persisted: %v", fx.app(0))
		}
	})
}

// ---- delete() ----

func TestDeleteCascadesDomains(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "delete-me", "active": true, "type": "container",
	}})

	r := fx.m.Delete(float64(1), true)
	if !r.Status {
		t.Fatalf("delete failed: %v", r.Message)
	}
	fx.domains.mu.Lock()
	defer fx.domains.mu.Unlock()
	if len(fx.domains.deleted) != 1 || fx.domains.deleted[0] != "delete-me" {
		t.Fatalf("domain cascade = %v", fx.domains.deleted)
	}
	if fx.appCount() != 0 {
		t.Fatalf("app not removed")
	}
	fx.dock.mu.Lock()
	defer fx.dock.mu.Unlock()
	if len(fx.dock.removedImages) != 1 || fx.dock.removedImages[0] != "odac-app-delete-me" {
		t.Fatalf("app image not removed: %v", fx.dock.removedImages)
	}
}

// errTest is a reusable sentinel for fake failures.
var errTest = errors.New("Container start failed")
