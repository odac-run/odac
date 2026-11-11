package docker

import (
	"errors"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
)

func TestNormalizeNetworks(t *testing.T) {
	got, err := NormalizeNetworks([]string{"web", "db", "web"})
	if err != nil || !reflect.DeepEqual(got, []string{"db", "web"}) {
		t.Fatalf("got %v, %v", got, err)
	}

	// Empty is legal (the private fallback) and must stay non-nil, since nil
	// is what RunOptions reads as "never set".
	if got, err := NormalizeNetworks(nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty list: %v, %v", got, err)
	}
	// Namespace selectors would bypass the networkMode guards or strand the
	// container without an address; the private network is ODAC's alone.
	for _, bad := range []string{"host", "HOST", "none", "default", PrivateNetwork, "", "-x", "a b", "container:web"} {
		if _, err := NormalizeNetworks([]string{"ok", bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRunAppJoinsPersistedNetworks(t *testing.T) {
	f := newFakeAPI()
	f.images["img"] = image.InspectResponse{}
	c := newTestClient(t, f)
	if _, err := c.RunApp("web", RunOptions{Image: "img", Networks: []string{"shared", "db-net", "shared"}}, nil, nil); err != nil {
		t.Fatal(err)
	}

	call := f.created[0]
	if got := string(call.HostConfig.NetworkMode); got != "db-net" {
		t.Errorf("network mode = %q, want db-net", got)
	}
	if !reflect.DeepEqual(f.netConnects, [][2]string{{"shared", call.ID}}) {
		t.Errorf("connects = %v", f.netConnects)
	}
	for _, n := range f.networks {
		if n == DefaultNetwork {
			t.Errorf("%s ensured for an app that left it", DefaultNetwork)
		}
	}
	if len(f.started) != 1 {
		t.Errorf("started = %v", f.started)
	}
}

// A container missing a network it was given must not start: the green
// container of a deploy would otherwise take traffic without its peers.
func TestRunAppConnectFailureRemovesContainer(t *testing.T) {
	f := newFakeAPI()
	f.images["img"] = image.InspectResponse{}
	f.netConnectErr = errors.New("pool exhausted")
	c := newTestClient(t, f)
	ok, err := c.RunApp("web", RunOptions{Image: "img", Networks: []string{"a", "b"}}, nil, nil)
	if ok || err == nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(f.started) != 0 {
		t.Errorf("started = %v", f.started)
	}
	if len(f.removed) == 0 || f.removed[len(f.removed)-1] != f.created[0].ID {
		t.Errorf("created container not removed: %v", f.removed)
	}
}

func TestRunAppNetworksFallbacks(t *testing.T) {
	run := func(t *testing.T, opts RunOptions) (*fakeAPI, createCall) {
		f := newFakeAPI()
		f.images["img"] = image.InspectResponse{}
		c := newTestClient(t, f)
		opts.Image = "img"
		if _, err := c.RunApp("web", opts, nil, nil); err != nil {
			t.Fatal(err)
		}
		return f, f.created[0]
	}

	t.Run("no list keeps the default", func(t *testing.T) {
		_, call := run(t, RunOptions{})
		if got := string(call.HostConfig.NetworkMode); got != DefaultNetwork {
			t.Errorf("network mode = %q", got)
		}
	})

	// Only a hand-edited record reaches RunApp with a reserved name.
	t.Run("unusable list degrades to the default", func(t *testing.T) {
		f, call := run(t, RunOptions{Networks: []string{"db", "host"}})
		if got := string(call.HostConfig.NetworkMode); got != DefaultNetwork {
			t.Errorf("network mode = %q", got)
		}
		if len(f.netConnects) != 0 {
			t.Errorf("connects = %v", f.netConnects)
		}
	})

	t.Run("isolation ignores the list", func(t *testing.T) {
		f, call := run(t, RunOptions{Isolated: true, Networks: []string{"db"}})
		if got := string(call.HostConfig.NetworkMode); got != IsolatedNetwork {
			t.Errorf("network mode = %q", got)
		}
		if len(f.netConnects) != 0 {
			t.Errorf("isolated app connected to %v", f.netConnects)
		}
	})

	t.Run("host mode ignores the list", func(t *testing.T) {
		f, call := run(t, RunOptions{NetworkMode: "host", Networks: []string{"db"}})
		if got := string(call.HostConfig.NetworkMode); got != "host" {
			t.Errorf("network mode = %q", got)
		}
		if len(f.netConnects) != 0 {
			t.Errorf("host app connected to %v", f.netConnects)
		}
	})
}

// Disconnecting first and then failing to connect would leave the container
// with no network at all, unreachable until its next restart.
func TestSetNetworksConnectsBeforeDisconnecting(t *testing.T) {
	f := newFakeAPI()
	f.networks = []string{"old"}
	f.netConnectErr = errors.New("pool exhausted")
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{"old": {}},
		},
	}
	c := newTestClient(t, f)

	if res := c.SetNetworks("web", []string{"new"}); res.Success {
		t.Fatal("expected failure")
	}
	if len(f.netDisconnects) != 0 {
		t.Errorf("disconnected before the connect succeeded: %v", f.netDisconnects)
	}
}

// Removing every network must leave the app reachable by the proxy, online,
// and unreachable from every other container.
func TestRunAppEmptyListJoinsPrivateNetwork(t *testing.T) {
	f := newFakeAPI()
	f.images["img"] = image.InspectResponse{}
	c := newTestClient(t, f)
	if _, err := c.RunApp("web", RunOptions{Image: "img", Networks: []string{}}, nil, nil); err != nil {
		t.Fatal(err)
	}

	if got := string(f.created[0].HostConfig.NetworkMode); got != PrivateNetwork {
		t.Errorf("network mode = %q, want %q", got, PrivateNetwork)
	}
	opts, ok := f.netCreateOpts[PrivateNetwork]
	if !ok {
		t.Fatalf("%s never created: %v", PrivateNetwork, f.networks)
	}
	if opts.Options[iccOption] != "false" {
		t.Error("private network created with inter-container traffic allowed")
	}
	if opts.Internal {
		t.Error("private network created internal: the app would lose its egress")
	}
	if len(f.netConnects) != 0 {
		t.Errorf("connects = %v", f.netConnects)
	}
}

func TestSetNetworksEmptyListMovesToPrivate(t *testing.T) {
	f := newFakeAPI()
	f.networks = []string{DefaultNetwork}
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{DefaultNetwork: {}},
		},
	}
	c := newTestClient(t, f)

	res := c.SetNetworks("web", []string{})
	if !res.Success {
		t.Fatalf("res = %+v", res)
	}
	if !reflect.DeepEqual(f.netConnects, [][2]string{{PrivateNetwork, "web"}}) {
		t.Errorf("connects = %v", f.netConnects)
	}
	if !reflect.DeepEqual(f.netDisconnects, [][2]string{{DefaultNetwork, "web"}}) {
		t.Errorf("disconnects = %v", f.netDisconnects)
	}
	if f.netCreateOpts[PrivateNetwork].Options[iccOption] != "false" {
		t.Error("private network not created with inter-container traffic off")
	}
}

// The private network is ODAC's placement: app.list and every other report
// read GetStatus, and none of them may name it.
func TestGetStatusHidesPrivateNetwork(t *testing.T) {
	f := newFakeAPI()
	f.inspects["web"] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State:      &container.State{Running: true},
			HostConfig: &container.HostConfig{NetworkMode: PrivateNetwork},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{PrivateNetwork: {}},
		},
	}
	c := newTestClient(t, f)
	if got := c.GetStatus("web").Networks; len(got) != 0 {
		t.Errorf("networks = %v", got)
	}
}
