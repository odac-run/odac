package kernel

import (
	"reflect"
	"testing"
)

func TestParseAbsent(t *testing.T) {
	for _, def := range []any{
		nil,
		map[string]any{},
		map[string]any{"image": "nginx"},
		map[string]any{"caps": nil, "sysctls": nil},
		map[string]any{"caps": []any{}, "sysctls": map[string]any{}},
		"not-an-object",
	} {
		spec, err := Parse(def)
		if err != nil {
			t.Errorf("Parse(%#v) errored: %s", def, err)
		}
		if spec != nil {
			t.Errorf("Parse(%#v) = %#v, want nil (nothing requested)", def, spec)
		}
	}
}

func TestParseCaps(t *testing.T) {
	spec, err := Parse(map[string]any{"caps": []any{"cap_net_admin", "NET_ADMIN", "NET_RAW"}})
	if err != nil {
		t.Fatal(err)
	}
	// Canonical, deduplicated, sorted: a re-parse of a persisted record must
	// produce the same bytes.
	if !reflect.DeepEqual(spec.Caps, []string{"NET_ADMIN", "NET_RAW"}) {
		t.Errorf("caps = %#v", spec.Caps)
	}
	if spec.Sysctls != nil {
		t.Errorf("sysctls = %#v, want nil", spec.Sysctls)
	}
}

func TestParseCapsRejected(t *testing.T) {
	// Every one of these crosses the container boundary or is malformed.
	for _, caps := range []any{
		[]any{"SYS_ADMIN"},
		[]any{"SYS_MODULE"},
		[]any{"CAP_SYS_RAWIO"},
		[]any{"DAC_READ_SEARCH"},
		[]any{"NET_ADMIN", "SYS_PTRACE"},
		[]any{""},
		[]any{42.0},
		"NET_ADMIN",
		map[string]any{"NET_ADMIN": true},
	} {
		if _, err := Parse(map[string]any{"caps": caps}); err == nil {
			t.Errorf("Parse(caps: %#v) accepted", caps)
		}
	}
}

func TestParseSysctls(t *testing.T) {
	spec, err := Parse(map[string]any{"sysctls": map[string]any{
		"net.ipv4.ip_forward":              1.0,
		"net.ipv4.conf.all.src_valid_mark": "1",
		"net.ipv6.conf.all.forwarding":     true,
		"kernel.shmmax":                    "67108864",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"net.ipv4.ip_forward":              "1",
		"net.ipv4.conf.all.src_valid_mark": "1",
		"net.ipv6.conf.all.forwarding":     "1",
		"kernel.shmmax":                    "67108864",
	}
	if !reflect.DeepEqual(spec.Sysctls, want) {
		t.Errorf("sysctls = %#v", spec.Sysctls)
	}
}

func TestParseSysctlsRejected(t *testing.T) {
	for _, sysctls := range []any{
		map[string]any{"vm.max_map_count": "262144"}, // host-wide, not namespaced
		map[string]any{"kernel.hostname": "x"},
		map[string]any{"fs.file-max": "1000"},
		map[string]any{"net..ipv4": "1"},
		map[string]any{"../../etc/passwd": "1"},
		map[string]any{"NET.IPV4.IP_FORWARD": "1"},
		map[string]any{"net.ipv4.ip_forward": ""},
		map[string]any{"net.ipv4.ip_forward": "1\nnet.ipv4.conf.all.forwarding=1"},
		map[string]any{"net.ipv4.ip_forward": []any{"1"}},
		[]any{"net.ipv4.ip_forward"},
	} {
		if _, err := Parse(map[string]any{"sysctls": sysctls}); err == nil {
			t.Errorf("Parse(sysctls: %#v) accepted", sysctls)
		}
	}
}

func TestParseLimits(t *testing.T) {
	caps := make([]any, maxCaps+1)
	for i := range caps {
		caps[i] = CapNetAdmin
	}
	if _, err := Parse(map[string]any{"caps": caps}); err == nil {
		t.Error("oversized caps array accepted")
	}
	sysctls := map[string]any{}
	for i := 0; i < maxSysctls+1; i++ {
		sysctls["net.ipv4.conf.eth"+string(rune('a'+i%26))+string(rune('a'+i/26))+".forwarding"] = "1"
	}
	if _, err := Parse(map[string]any{"sysctls": sysctls}); err == nil {
		t.Error("oversized sysctls object accepted")
	}
	long := make([]byte, maxSysctlValue+1)
	for i := range long {
		long[i] = '1'
	}
	if _, err := Parse(map[string]any{"sysctls": map[string]any{"net.ipv4.ip_forward": string(long)}}); err == nil {
		t.Error("oversized sysctl value accepted")
	}
}

func TestNetSysctls(t *testing.T) {
	spec := &Spec{
		Caps: []string{CapNetAdmin},
		Sysctls: map[string]string{
			"net.ipv6.conf.all.forwarding": "1",
			"net.ipv4.ip_forward":          "1",
			"kernel.shmmax":                "1024",
		},
	}
	if got := spec.NetSysctls(); !reflect.DeepEqual(got, []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"}) {
		t.Errorf("NetSysctls() = %#v", got)
	}
	stripped := spec.WithoutNetSysctls()
	if !reflect.DeepEqual(stripped.Sysctls, map[string]string{"kernel.shmmax": "1024"}) {
		t.Errorf("stripped sysctls = %#v", stripped.Sysctls)
	}
	if !reflect.DeepEqual(stripped.Caps, []string{CapNetAdmin}) {
		t.Errorf("stripping net sysctls must not touch caps, got %#v", stripped.Caps)
	}
	// The original is untouched: SetNetworkMode reports what it dropped.
	if len(spec.Sysctls) != 3 {
		t.Errorf("WithoutNetSysctls mutated the receiver: %#v", spec.Sysctls)
	}
	var nilSpec *Spec
	if nilSpec.NetSysctls() != nil || nilSpec.WithoutNetSysctls() != nil {
		t.Error("nil spec must answer without a nil check")
	}
	if got := (&Spec{Sysctls: map[string]string{"net.ipv4.ip_forward": "1"}}).WithoutNetSysctls(); got != nil {
		t.Errorf("a request that was only net sysctls must strip to nil, got %#v", got)
	}
}

// An app that asked for nothing must not grow an empty field: the Cloud
// compares app.list rows by serialization.
func TestApply(t *testing.T) {
	app := map[string]any{"name": "wg"}
	(&Spec{Caps: []string{CapNetAdmin}, Sysctls: map[string]string{"net.ipv4.ip_forward": "1"}}).Apply(app)
	if !reflect.DeepEqual(app["caps"], []any{CapNetAdmin}) {
		t.Errorf("caps = %#v", app["caps"])
	}
	if !reflect.DeepEqual(app["sysctls"], map[string]any{"net.ipv4.ip_forward": "1"}) {
		t.Errorf("sysctls = %#v", app["sysctls"])
	}
	// Round-trips: a persisted record re-parses to the same spec.
	spec, err := Parse(app)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Caps, []string{CapNetAdmin}) || spec.Sysctls["net.ipv4.ip_forward"] != "1" {
		t.Errorf("round-trip = %#v", spec)
	}

	var none *Spec
	none.Apply(app)
	if _, ok := app["caps"]; ok {
		t.Error("caps left on an app that requests none")
	}
	if _, ok := app["sysctls"]; ok {
		t.Error("sysctls left on an app that requests none")
	}
	plain := map[string]any{"name": "web"}
	none.Apply(plain)
	if !reflect.DeepEqual(plain, map[string]any{"name": "web"}) {
		t.Errorf("an app requesting nothing must stay byte-identical, got %#v", plain)
	}
}
