package updater

// Test doubles mirroring test/server/Updater.test.js's harness: a stateful
// fake Docker daemon (a map of container name -> {policy, running, env,
// binds}) instead of per-call mocks, because the bugs this suite guards
// against are about *sequences* of Docker operations producing an unsafe
// intermediate state. Real unix sockets are used for the handshake tests.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/go-connections/nat"

	"odac/internal/logx"
)

type fakeContainer struct {
	policy       string
	running      bool
	env          []string
	binds        []string
	image        string
	mounts       []MountPoint
	portBindings nat.PortMap
}

type fakeWorld struct {
	mu      sync.Mutex
	world   map[string]*fakeContainer
	idIndex map[string]string // fake container id -> name (identity-probe tests)
	events  []string
	pulled  []string
	anonSeq int

	imageIDs map[string]string            // ImageID per tag; missing -> not found
	files    map[string]map[string][]byte // ReadFile per name -> path -> content
	pullErr  map[string]error
	waitCode map[string]int64

	// onEvent runs under mu right after a mutating call records its event
	// (the jest suite's wrapped events.push) — it may read the maps
	// directly but must not call fake methods.
	onEvent func(event string)

	// onCreate runs after a container is created (build-from-source tests
	// simulate the sidecar's clone side effect here). Runs outside mu.
	onCreate func(name string, opts CreateOptions)
}

func newFakeWorld() *fakeWorld {
	return &fakeWorld{
		world:    map[string]*fakeContainer{},
		idIndex:  map[string]string{},
		imageIDs: map[string]string{},
		files:    map[string]map[string][]byte{},
		pullErr:  map[string]error{},
		waitCode: map[string]int64{},
	}
}

func notFoundErr(key string) error {
	return fmt.Errorf("No such container: %s: %w", key, cerrdefs.ErrNotFound)
}

// resolve maps a name or fake id to the world key, like the jest fake.
func (w *fakeWorld) resolve(key string) (string, bool) {
	if _, ok := w.world[key]; ok {
		return key, true
	}
	if name, ok := w.idIndex[key]; ok {
		if _, ok := w.world[name]; ok {
			return name, true
		}
	}
	return "", false
}

func (w *fakeWorld) push(event string) {
	w.events = append(w.events, event)
	if w.onEvent != nil {
		w.onEvent(event)
	}
}

func (w *fakeWorld) Inspect(name string) (ContainerInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	resolved, ok := w.resolve(name)
	if !ok {
		return ContainerInfo{}, notFoundErr(name)
	}
	c := w.world[resolved]
	return ContainerInfo{
		ID:            resolved,
		Name:          "/" + resolved,
		Env:           append([]string(nil), c.env...),
		Binds:         append([]string(nil), c.binds...),
		RestartPolicy: c.policy,
		Running:       c.running,
		Image:         c.image,
		PortBindings:  c.portBindings,
		Mounts:        append([]MountPoint(nil), c.mounts...),
	}, nil
}

func (w *fakeWorld) Create(opts CreateOptions) (string, error) {
	w.mu.Lock()
	name := opts.Name
	if name == "" {
		w.anonSeq++
		name = fmt.Sprintf("anon%d", w.anonSeq)
	}
	w.world[name] = &fakeContainer{
		policy:       opts.RestartPolicy,
		running:      false,
		env:          append([]string(nil), opts.Env...),
		binds:        append([]string(nil), opts.Binds...),
		portBindings: opts.PortBindings,
	}
	w.push(fmt.Sprintf("create:%s:policy=%s", name, opts.RestartPolicy))
	w.mu.Unlock()
	if w.onCreate != nil {
		w.onCreate(name, opts)
	}
	return name, nil
}

func (w *fakeWorld) Start(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if resolved, ok := w.resolve(name); ok {
		w.world[resolved].running = true
		w.push("start:" + resolved)
		return nil
	}
	w.push("start:" + name)
	return nil // jest fake never fails start
}

func (w *fakeWorld) Stop(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if resolved, ok := w.resolve(name); ok {
		w.world[resolved].running = false
	}
	return nil
}

func (w *fakeWorld) Rename(name, newName string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	resolved, ok := w.resolve(name)
	if !ok {
		return notFoundErr(name)
	}
	if _, exists := w.world[newName]; exists {
		return fmt.Errorf("name %s already in use", newName)
	}
	w.world[newName] = w.world[resolved]
	delete(w.world, resolved)
	w.push(fmt.Sprintf("rename:%s->%s", resolved, newName))
	return nil
}

func (w *fakeWorld) Remove(name string, force bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	resolved, ok := w.resolve(name)
	if !ok {
		return notFoundErr(name)
	}
	delete(w.world, resolved)
	w.push("remove:" + resolved)
	return nil
}

func (w *fakeWorld) UpdateRestartPolicy(name, policy string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	resolved, ok := w.resolve(name)
	if !ok {
		return notFoundErr(name)
	}
	w.world[resolved].policy = policy
	w.push(fmt.Sprintf("policy:%s=%s", resolved, policy))
	return nil
}

func (w *fakeWorld) Wait(name string) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waitCode[name], nil
}

func (w *fakeWorld) Logs(name string, follow bool, tail string) (io.ReadCloser, error) {
	return nil, errors.New("no logs available in test harness")
}

func (w *fakeWorld) Pull(imageRef string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pulled = append(w.pulled, imageRef)
	return w.pullErr[imageRef]
}

func (w *fakeWorld) ImageID(imageRef string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id, ok := w.imageIDs[imageRef]; ok {
		return id, nil
	}
	return "", notFoundErr(imageRef)
}

func (w *fakeWorld) ReadFile(name, path string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if byPath, ok := w.files[name]; ok {
		if content, ok := byPath[path]; ok {
			return content, nil
		}
	}
	return nil, errors.New("exec disabled in tests")
}

func (w *fakeWorld) eventList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

func (w *fakeWorld) container(name string) *fakeContainer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.world[name]
}

func (w *fakeWorld) has(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.world[name]
	return ok
}

func (w *fakeWorld) hasEvent(event string) bool {
	for _, e := range w.eventList() {
		if e == event {
			return true
		}
	}
	return false
}

func (w *fakeWorld) hasEventPrefix(prefix string) bool {
	for _, e := range w.eventList() {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// fakeSystem records Stop/Init calls; Init may be pointed at the real
// Updater.Init (the jest suite's self-handshake regression setup).
type fakeSystem struct {
	mu     sync.Mutex
	calls  []string
	initFn func() error
}

func (s *fakeSystem) Init() error {
	s.mu.Lock()
	s.calls = append(s.calls, "init")
	fn := s.initFn
	s.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return nil
}

func (s *fakeSystem) Stop(exceptWeb bool) {
	s.mu.Lock()
	s.calls = append(s.calls, fmt.Sprintf("stop:exceptWeb=%v", exceptWeb))
	s.mu.Unlock()
}

func (s *fakeSystem) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// fakeWeb is the Proxy/DNS stand-in for the handover overlap.
type fakeWeb struct {
	mu      sync.Mutex
	name    string
	ready   bool
	stopped int
}

func (f *fakeWeb) Stop() {
	f.mu.Lock()
	f.stopped++
	f.mu.Unlock()
}

func (f *fakeWeb) WaitForReady(time.Duration) bool { return f.ready }

func (f *fakeWeb) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// fixture builds an Updater over the fake world with test-friendly probes
// and timings (the load-bearing production values stay in New).
type fixture struct {
	u     *Updater
	w     *fakeWorld
	sys   *fakeSystem
	proxy *fakeWeb
	dns   *fakeWeb

	mu          sync.Mutex
	hostnameVal string
	procFiles   map[string]string
	dockerenv   bool
	exitCodes   []int
	exited      chan int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{
		w:           newFakeWorld(),
		sys:         &fakeSystem{},
		proxy:       &fakeWeb{name: "proxy", ready: true},
		dns:         &fakeWeb{name: "dns", ready: true},
		hostnameVal: "test-host", // never matches the 12-hex short-id probe
		procFiles:   map[string]string{},
		dockerenv:   true,
		exited:      make(chan int, 4),
	}

	// Unix socket paths are capped at ~104 bytes on darwin — t.TempDir()
	// paths overflow that, so use /tmp like the jest suite does.
	base, err := os.MkdirTemp("/tmp", "odac-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if err := os.MkdirAll(filepath.Join(base, "run"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := New(base, Deps{Docker: fx.w, Proxy: fx.proxy, DNS: fx.dns})
	u.SetSystem(fx.sys)
	u.platform = "linux"
	u.hostname = func() string {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.hostnameVal
	}
	u.readFile = func(p string) ([]byte, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		if content, ok := fx.procFiles[p]; ok {
			return []byte(content), nil
		}
		return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
	}
	u.inContainer = func() bool {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.dockerenv
	}
	u.exit = func(code int) {
		fx.mu.Lock()
		fx.exitCodes = append(fx.exitCodes, code)
		fx.mu.Unlock()
		fx.exited <- code
	}
	// Short timings so failure paths resolve quickly; individual tests
	// override where a specific timer is under test.
	u.handshakeTimeout = 3 * time.Second
	u.globalTimeout = 5 * time.Second
	u.stabilityDelay = 30 * time.Millisecond
	u.destructDelay = 20 * time.Millisecond
	u.readyTimeout = 50 * time.Millisecond
	fx.u = u
	return fx
}

func (fx *fixture) setHostname(h string) {
	fx.mu.Lock()
	fx.hostnameVal = h
	fx.mu.Unlock()
}

func (fx *fixture) setProcFile(path, content string) {
	fx.mu.Lock()
	fx.procFiles[path] = content
	fx.mu.Unlock()
}

func (fx *fixture) setDockerenv(v bool) {
	fx.mu.Lock()
	fx.dockerenv = v
	fx.mu.Unlock()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitUntil: timed out waiting for %s", what)
}

// captureStdout swaps logx.Stdout for the test's lifetime and returns a
// getter for everything written (the SWITCH_LOGS marker assertions).
func captureStdout(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	old := logx.Stdout
	logx.Stdout = writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	t.Cleanup(func() { logx.Stdout = old })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
