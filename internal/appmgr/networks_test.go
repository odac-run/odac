package appmgr

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"odac/internal/docker"
)

func newNetworksFixture(t *testing.T, extra map[string]any) *fixture {
	app := map[string]any{"id": float64(1), "name": "net-app", "type": "container", "image": "test:latest"}
	for k, v := range extra {
		app[k] = v
	}
	fx := newFixture(t, []any{app})
	fx.dock.networksResult = docker.SetNetworksResult{Success: true}
	return fx
}

func (fx *fixture) syncCount() int {
	fx.proxy.mu.Lock()
	defer fx.proxy.mu.Unlock()
	return fx.proxy.syncs
}

func TestSetNetworksPersists(t *testing.T) {
	// The bug this guards: the list only reached the live container, so the
	// next restart recreated the app on odac-network alone.
	t.Run("stopped app saves the list for its next start", func(t *testing.T) {
		fx := newNetworksFixture(t, nil)
		r := fx.m.SetNetworks("net-app", []any{"web", "db", "web"}, true)
		if !r.Status {
			t.Fatalf("refused: %v", r.Message)
		}
		if got := fx.app(0)["networks"]; !reflect.DeepEqual(got, []any{"db", "web"}) {
			t.Errorf("persisted = %v", got)
		}
		if len(fx.dock.networksCalls) != 0 {
			t.Errorf("live reconcile on a stopped app: %v", fx.dock.networksCalls)
		}
		if !reflect.DeepEqual(fx.dock.ensuredNets, [][]string{{"db", "web"}}) {
			t.Errorf("ensured = %v", fx.dock.ensuredNets)
		}
		if fx.syncCount() != 0 {
			t.Error("proxy synced for a stopped app")
		}
		if !strings.Contains(jsString(r.Message), "next start") {
			t.Errorf("message = %q", jsString(r.Message))
		}
	})

	t.Run("running app is reconciled live and the proxy resyncs", func(t *testing.T) {
		fx := newNetworksFixture(t, map[string]any{"activeContainerId": "net-app-green-1"})
		fx.dock.running["net-app-green-1"] = true
		r := fx.m.SetNetworks("net-app", []any{"db"}, true)
		if !r.Status {
			t.Fatalf("refused: %v", r.Message)
		}
		if !reflect.DeepEqual(fx.dock.networksCalls, [][]string{{"net-app-green-1", "db"}}) {
			t.Errorf("live calls = %v", fx.dock.networksCalls)
		}
		if got := fx.app(0)["networks"]; !reflect.DeepEqual(got, []any{"db"}) {
			t.Errorf("persisted = %v", got)
		}
		// The address moved; a stale one may already belong to another app.
		if fx.syncCount() != 1 {
			t.Errorf("proxy syncs = %d", fx.syncCount())
		}
	})

	// app.list rows are diffed by serialization, so an app back on the
	// default must look exactly like one that never left it.
	t.Run("default list clears the field", func(t *testing.T) {
		fx := newNetworksFixture(t, map[string]any{"networks": []any{"db"}})
		if r := fx.m.SetNetworks("net-app", []any{docker.DefaultNetwork}, true); !r.Status {
			t.Fatalf("refused: %v", r.Message)
		}
		if _, ok := fx.app(0)["networks"]; ok {
			t.Errorf("networks kept: %v", fx.app(0)["networks"])
		}
	})

	t.Run("live failure persists nothing", func(t *testing.T) {
		fx := newNetworksFixture(t, nil)
		fx.dock.running["net-app"] = true
		fx.dock.networksResult = docker.SetNetworksResult{Success: false, Message: "boom"}
		if r := fx.m.SetNetworks("net-app", []any{"db"}, true); r.Status {
			t.Fatal("accepted")
		}
		if _, ok := fx.app(0)["networks"]; ok {
			t.Error("list persisted after a failed reconcile")
		}
	})

	t.Run("network creation failure persists nothing", func(t *testing.T) {
		fx := newNetworksFixture(t, nil)
		fx.dock.ensureNetsErr = errors.New("pool exhausted")
		if r := fx.m.SetNetworks("net-app", []any{"db"}, true); r.Status {
			t.Fatal("accepted")
		}
		if _, ok := fx.app(0)["networks"]; ok {
			t.Error("list persisted after network creation failed")
		}
	})

	// The Hub's Save & Restart sends the list and app.restart together; the
	// restart winning the lock used to drop the list without a word.
	t.Run("saved while a restart holds the app, applied after it", func(t *testing.T) {
		fx := newNetworksFixture(t, map[string]any{"networks": []any{}})
		fx.dock.running["net-app"] = true
		fx.m.tryLockProcessing(1)
		waits := 0
		fx.m.sleep = func(time.Duration) {
			if waits++; waits == 3 {
				fx.m.unlockProcessing(1)
			}
		}
		r := fx.m.SetNetworks("net-app", []any{docker.DefaultNetwork}, true)
		if !r.Status {
			t.Fatalf("refused: %v", r.Message)
		}
		if _, ok := fx.app(0)["networks"]; ok {
			t.Errorf("record not back on the default: %v", fx.app(0)["networks"])
		}
		fx.waitIdle(t)
		if !reflect.DeepEqual(fx.dock.networksCalls, [][]string{{"net-app", docker.DefaultNetwork}}) {
			t.Errorf("live calls = %v", fx.dock.networksCalls)
		}
		if fx.syncCount() != 1 {
			t.Errorf("proxy syncs = %d", fx.syncCount())
		}
	})

	t.Run("one follower per app, reading the latest list", func(t *testing.T) {
		fx := newNetworksFixture(t, nil)
		fx.dock.running["net-app"] = true
		fx.m.tryLockProcessing(1)
		gate := make(chan struct{})
		fx.m.sleep = func(time.Duration) { <-gate }
		fx.m.SetNetworks("net-app", []any{"a"}, true)
		fx.m.SetNetworks("net-app", []any{"b"}, true)
		fx.m.unlockProcessing(1)
		close(gate)
		fx.waitIdle(t)
		if !reflect.DeepEqual(fx.dock.networksCalls, [][]string{{"net-app", "b"}}) {
			t.Errorf("live calls = %v", fx.dock.networksCalls)
		}
	})
}

func TestSetNetworksRejects(t *testing.T) {
	cases := map[string][]any{
		"host":       {"db", "host"},
		"private":    {docker.PrivateNetwork},
		"none":       {"none"},
		"non-string": {"db", float64(1)},
		"bad name":   {"-db"},
	}
	for label, list := range cases {
		t.Run(label, func(t *testing.T) {
			fx := newNetworksFixture(t, map[string]any{"networks": []any{"keep"}})
			if r := fx.m.SetNetworks("net-app", list, true); r.Status {
				t.Fatalf("accepted %v", list)
			}
			if got := fx.app(0)["networks"]; !reflect.DeepEqual(got, []any{"keep"}) {
				t.Errorf("record changed to %v", got)
			}
		})
	}
}

// Removing every network is a request to share none, not an error: the app
// lands on docker's private network, and no reply may name that network.
func TestSetNetworksEmptyList(t *testing.T) {
	t.Run("stopped app keeps an empty list", func(t *testing.T) {
		fx := newNetworksFixture(t, nil)
		r := fx.m.SetNetworks("net-app", []any{}, true)
		if !r.Status {
			t.Fatalf("refused: %v", r.Message)
		}
		got, ok := fx.app(0)["networks"].([]any)
		if !ok || len(got) != 0 {
			t.Errorf("persisted = %#v, want an empty list", fx.app(0)["networks"])
		}
		if strings.Contains(jsString(r.Message), docker.PrivateNetwork) {
			t.Errorf("reply names the private network: %q", jsString(r.Message))
		}
	})

	t.Run("running app is reconciled with the empty list", func(t *testing.T) {
		fx := newNetworksFixture(t, nil)
		fx.dock.running["net-app"] = true
		r := fx.m.SetNetworks("net-app", []any{}, true)
		if !r.Status {
			t.Fatalf("refused: %v", r.Message)
		}
		if !reflect.DeepEqual(fx.dock.networksCalls, [][]string{{"net-app"}}) {
			t.Errorf("live calls = %v", fx.dock.networksCalls)
		}
		if strings.Contains(jsString(r.Message), docker.PrivateNetwork) {
			t.Errorf("reply names the private network: %q", jsString(r.Message))
		}
		if fx.syncCount() != 1 {
			t.Errorf("proxy syncs = %d", fx.syncCount())
		}
	})

	// nil reads as "never set" (the default network) on the way to RunApp,
	// so the persisted empty list must arrive non-nil.
	t.Run("reaches RunOptions as empty, not nil", func(t *testing.T) {
		fx := newFixture(t, []any{map[string]any{
			"id": float64(1), "name": "n-app", "active": true,
			"type": "container", "image": "test:latest", "networks": []any{},
		}})
		fx.checkAndSettle(t)
		if fx.dock.runCallCount() == 0 {
			t.Fatal("runApp not called")
		}
		got := fx.dock.runCallAt(0).options.Networks
		if got == nil || len(got) != 0 {
			t.Errorf("RunOptions.Networks = %#v", got)
		}
	})
}

func TestNetworksAppliedToRunOptions(t *testing.T) {
	app := map[string]any{
		"id": float64(1), "name": "n-app", "active": true,
		"type": "container", "image": "test:latest",
		"networks": []any{"db", "odac-network"},
	}
	fx := newFixture(t, []any{app})
	fx.checkAndSettle(t)
	if fx.dock.runCallCount() == 0 {
		t.Fatal("runApp not called")
	}
	if got := fx.dock.runCallAt(0).options.Networks; !reflect.DeepEqual(got, []string{"db", "odac-network"}) {
		t.Errorf("RunOptions.Networks = %v", got)
	}

	plain := newFixture(t, []any{map[string]any{
		"id": float64(1), "name": "p-app", "active": true,
		"type": "container", "image": "test:latest",
	}})
	plain.checkAndSettle(t)
	if got := plain.dock.runCallAt(0).options.Networks; got != nil {
		t.Errorf("app with no list got %#v, want nil (the default network)", got)
	}
}
