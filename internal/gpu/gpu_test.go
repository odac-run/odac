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
