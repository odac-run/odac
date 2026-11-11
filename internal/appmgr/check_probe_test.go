package appmgr

import (
	"errors"
	"testing"
)

// TestCheckDoesNotRestartWhenDockerCannotAnswer: a failed container list is
// an unknown state. Reading it as "stopped" recreated every app on every
// pulse while the daemon was stalled.
func TestCheckDoesNotRestartWhenDockerCannotAnswer(t *testing.T) {
	fx := newFixture(t, []any{map[string]any{
		"id": float64(7), "name": "web", "active": true,
		"type": "container", "status": "running",
	}})
	fx.dock.mu.Lock()
	fx.dock.listErr = errors.New("context deadline exceeded")
	fx.dock.mu.Unlock()

	fx.checkAndSettle(t)
	fx.checkAndSettle(t)
	if got := fx.dock.runCallCount(); got != 0 {
		t.Fatalf("%d runs while Docker could not answer, want 0", got)
	}

	// Once Docker answers and the container is really gone, it restarts.
	fx.dock.mu.Lock()
	fx.dock.listErr = nil
	fx.dock.mu.Unlock()
	fx.checkAndSettle(t)
	if got := fx.dock.runCallCount(); got != 1 {
		t.Fatalf("%d runs after Docker recovered, want 1", got)
	}
}

// TestCheckListsContainersOncePerPulse pins the pulse's daemon cost: one
// list for any number of apps, where it used to inspect each app.
func TestCheckListsContainersOncePerPulse(t *testing.T) {
	var apps []any
	for i := 1; i <= 20; i++ {
		apps = append(apps, map[string]any{
			"id": float64(i), "name": "app" + string(rune('a'+i)), "active": true,
			"type": "container", "status": "running",
		})
	}
	fx := newFixture(t, apps)
	counting := &countingDocker{fakeDocker: fx.dock}
	fx.dock.mu.Lock()
	for _, a := range apps {
		fx.dock.running[a.(map[string]any)["name"].(string)] = true
	}
	fx.dock.mu.Unlock()
	fx.m.deps.Docker = counting

	fx.checkAndSettle(t)
	if counting.lists != 1 || counting.inspects != 0 {
		t.Fatalf("lists=%d inspects=%d, want one list and no inspect", counting.lists, counting.inspects)
	}
	if got := fx.dock.runCallCount(); got != 0 {
		t.Fatalf("%d runs for running apps", got)
	}
}

type countingDocker struct {
	*fakeDocker
	lists, inspects int
}

func (c *countingDocker) RunningNames() (map[string]bool, error) {
	c.lists++
	return c.fakeDocker.RunningNames()
}

func (c *countingDocker) IsRunning(name string) bool {
	c.inspects++
	return c.fakeDocker.IsRunning(name)
}
