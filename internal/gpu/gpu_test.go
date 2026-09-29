package gpu

import (
	"encoding/json"
	"testing"
)

// decode mirrors how a Hub payload reaches Parse: through encoding/json, so
// every number is a float64 and every object a map[string]any.
func decode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseAccepts(t *testing.T) {
	cases := map[string]struct {
		payload string
		want    Spec
	}{
		"cloud single app": {
			`{"vendor":"nvidia","runtime":"nvidia","count":"all"}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll},
		},
		"numeric count": {
			`{"vendor":"nvidia","runtime":"nvidia","count":2}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: 2},
		},
		"numeric string count": {
			`{"runtime":"nvidia","count":"1"}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: 1},
		},
		"missing count means all": {
			`{"vendor":"nvidia","runtime":"nvidia"}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll},
		},
		"vendor derives runtime": {
			`{"vendor":"amd"}`,
			Spec{Vendor: VendorAMD, Runtime: RuntimeROCm, Count: CountAll},
		},
		"runtime derives vendor": {
			`{"runtime":"rocm"}`,
			Spec{Vendor: VendorAMD, Runtime: RuntimeROCm, Count: CountAll},
		},
		"case and padding tolerated": {
			`{"vendor":" NVIDIA ","runtime":"Nvidia","count":" All "}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll},
		},
		"intel": {
			`{"vendor":"intel","runtime":"intel","count":"all"}`,
			Spec{Vendor: VendorIntel, Runtime: RuntimeIntel, Count: CountAll},
		},
		"optional pins a runtime": {
			`{"runtime":"intel","optional":true}`,
			Spec{Vendor: VendorIntel, Runtime: RuntimeIntel, Count: CountAll, Optional: true},
		},
		"optional alone means auto": {
			`{"optional":true}`,
			Spec{Count: CountAll, Optional: true},
		},
		"optional auto keeps its count": {
			`{"optional":true,"count":2}`,
			Spec{Count: 2, Optional: true},
		},
		"optional as a string": {
			`{"runtime":"nvidia","optional":"true"}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll, Optional: true},
		},
		"optional false is the plain required form": {
			`{"runtime":"nvidia","optional":false}`,
			Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spec, err := Parse(decode(t, tc.payload))
			if err != nil {
				t.Fatalf("Parse(%s) = %v", tc.payload, err)
			}
			if spec == nil || *spec != tc.want {
				t.Fatalf("Parse(%s) = %+v, want %+v", tc.payload, spec, tc.want)
			}
		})
	}
}

// Absent is not an error: it is the ordinary CPU app.
func TestParseAbsent(t *testing.T) {
	for _, payload := range []any{nil, decode(t, "null"), decode(t, "{}"), map[string]any(nil)} {
		spec, err := Parse(payload)
		if err != nil || spec != nil {
			t.Errorf("Parse(%v) = (%+v, %v), want (nil, nil)", payload, spec, err)
		}
	}
}

// Malformed requests must fail loudly — silently dropping the GPU would run
// a CUDA-only image on the CPU and crash-loop it.
func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown runtime":  `{"vendor":"nvidia","runtime":"cuda"}`,
		"unknown vendor":   `{"vendor":"matrox"}`,
		"vendor mismatch":  `{"vendor":"amd","runtime":"nvidia"}`,
		"zero count":       `{"runtime":"nvidia","count":0}`,
		"negative count":   `{"runtime":"nvidia","count":-2}`,
		"fractional count": `{"runtime":"nvidia","count":1.5}`,
		"absurd count":     `{"runtime":"nvidia","count":9001}`,
		"garbage count":    `{"runtime":"nvidia","count":"lots"}`,
		"bool count":       `{"runtime":"nvidia","count":true}`,
		"not an object":    `"nvidia"`,
		"array":            `["nvidia"]`,
		"empty strings":    `{"vendor":"","runtime":""}`,
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			spec, err := Parse(decode(t, payload))
			if err == nil {
				t.Fatalf("Parse(%s) = %+v, want an error", payload, spec)
			}
		})
	}
}

// Map is what lands in apps.json and goes back to the Cloud in app.list, so
// it must round-trip through Parse unchanged.
func TestMapRoundTrip(t *testing.T) {
	for _, want := range []Spec{
		{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll},
		{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: 2},
		{Vendor: VendorAMD, Runtime: RuntimeROCm, Count: CountAll},
		{Vendor: VendorIntel, Runtime: RuntimeIntel, Count: CountAll, Optional: true},
		{Count: CountAll, Optional: true},
		{Count: 4, Optional: true},
	} {
		raw, err := json.Marshal(want.Map())
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(decode(t, string(raw)))
		if err != nil {
			t.Fatalf("re-parsing %s: %v", raw, err)
		}
		if got == nil || *got != want {
			t.Errorf("%s round-tripped to %+v, want %+v", raw, got, want)
		}
	}

	var nilSpec *Spec
	if nilSpec.Map() != nil || nilSpec.String() != "none" {
		t.Error("nil Spec must render as nothing")
	}
}

// The dashboard diffs app.list rows by serialization, so Map must not invent
// members. A required request keeps the exact shape it has always had, and an
// auto one carries no empty runtime/vendor to churn every row.
func TestMapOmitsAbsentFields(t *testing.T) {
	required := (&Spec{Vendor: VendorNvidia, Runtime: RuntimeNvidia, Count: CountAll}).Map()
	if _, ok := required["optional"]; ok {
		t.Errorf("a required request must not serialize an optional member: %v", required)
	}
	if len(required) != 3 {
		t.Errorf("required shape changed: %v", required)
	}

	auto := (&Spec{Count: CountAll, Optional: true}).Map()
	for _, key := range []string{"runtime", "vendor"} {
		if _, ok := auto[key]; ok {
			t.Errorf("an auto request must not serialize %q: %v", key, auto)
		}
	}
	if auto["optional"] != true {
		t.Errorf("auto request lost its optional member: %v", auto)
	}
}

// Auto is a shape only an optional request may take: a required one that
// resolves to nothing has no honest outcome left but a start failure.
func TestAutoRequiresOptional(t *testing.T) {
	if _, err := Parse(decode(t, `{"count":"all"}`)); err == nil {
		t.Error("a required request naming no runtime must be rejected")
	}

	spec, err := Parse(decode(t, `{"optional":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.IsAuto() {
		t.Errorf("%+v should report as auto", spec)
	}
	if got := spec.String(); got != "auto×all (optional)" {
		t.Errorf("String() = %q", got)
	}

	pinned, err := Parse(decode(t, `{"runtime":"rocm"}`))
	if err != nil {
		t.Fatal(err)
	}
	if pinned.IsAuto() {
		t.Error("a pinned runtime is not auto")
	}
}

func TestVendorFor(t *testing.T) {
	for runtime, want := range map[string]string{
		RuntimeNvidia: VendorNvidia,
		RuntimeROCm:   VendorAMD,
		RuntimeIntel:  VendorIntel,
		"":            "",
		"tpu":         "",
	} {
		if got := VendorFor(runtime); got != want {
			t.Errorf("VendorFor(%q) = %q, want %q", runtime, got, want)
		}
	}
}
