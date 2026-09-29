package resources

import "testing"

func TestParseAbsent(t *testing.T) {
	for _, def := range []any{
		nil,
		map[string]any{},
		map[string]any{"image": "nginx"},
		map[string]any{"shmSize": nil},
		map[string]any{"shmSize": ""},
		"not an object",
	} {
		spec, err := Parse(def)
		if err != nil {
			t.Fatalf("Parse(%v) = %v, want no error", def, err)
		}
		if spec != nil {
			t.Fatalf("Parse(%v) = %+v, want nil spec", def, spec)
		}
	}
}

func TestParseSizes(t *testing.T) {
	cases := map[string]int64{
		"512m":         512 << 20,
		"512M":         512 << 20,
		"512mb":        512 << 20,
		"512MiB":       512 << 20,
		"1g":           1 << 30,
		"1.5g":         3 << 29,
		"  2 GiB  ":    2 << 30,
		"1048576":      1 << 20,
		"4096k":        4 << 20,
		"268435456b":   256 << 20,
		"64GiB":        MaxShmSize,
		"1048576.0000": 1 << 20,
		"1024kib":      1 << 20,
		"0":            0,
		"0m":           0,
	}
	for input, want := range cases {
		spec, err := Parse(map[string]any{"shmSize": input})
		if err != nil {
			t.Fatalf("Parse(%q) = %v", input, err)
		}
		if want == 0 {
			if spec != nil {
				t.Fatalf("Parse(%q) = %+v, want nil spec", input, spec)
			}
			continue
		}
		if spec.Shm() != want {
			t.Fatalf("Parse(%q) = %d bytes, want %d", input, spec.Shm(), want)
		}
	}
}

// A dashboard that stores the canonical byte count sends it back as a JSON
// number; both shapes must mean the same thing.
func TestParseNumbers(t *testing.T) {
	spec, err := Parse(map[string]any{"shmSize": float64(512 << 20)})
	if err != nil || spec.Shm() != 512<<20 {
		t.Fatalf("float64 payload = %+v, %v", spec, err)
	}
	if spec, err := Parse(map[string]any{"shmSize": 1 << 20}); err != nil || spec.Shm() != 1<<20 {
		t.Fatalf("int payload = %+v, %v", spec, err)
	}
	if spec, err := Parse(map[string]any{"shmSize": float64(0)}); err != nil || spec != nil {
		t.Fatalf("zero payload = %+v, %v, want nil spec", spec, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, value := range []any{
		"512",                    // bytes, and almost certainly a missing unit
		512,                      // same, as a number
		-1,                       // negative
		float64(-512 << 20),      //
		"512tb",                  // unit outside the set
		"lots",                   // not a number
		"512 m b",                // not a size
		float64(MaxShmSize + 1),  // over the ceiling
		"128g",                   // over the ceiling
		1.5,                      // fractional bytes
		true,                     // wrong JSON type
		[]any{"512m"},            //
		map[string]any{"m": 512}, //
	} {
		if spec, err := Parse(map[string]any{"shmSize": value}); err == nil {
			t.Fatalf("Parse(%v) = %+v, want an error", value, spec)
		}
	}
}

// An app that asked for nothing must keep a byte-identical record: the Cloud
// diffs app.list rows by serialization.
func TestApply(t *testing.T) {
	app := map[string]any{"name": "frigate"}
	(*Spec)(nil).Apply(app)
	if _, present := app["shmSize"]; present {
		t.Fatalf("nil spec stamped a field: %v", app)
	}

	(&Spec{ShmSize: 512 << 20}).Apply(app)
	if app["shmSize"] != float64(512<<20) {
		t.Fatalf("Apply = %v, want %v", app["shmSize"], float64(512<<20))
	}

	// The persisted record re-parses to the same request.
	spec, err := Parse(app)
	if err != nil || spec.Shm() != 512<<20 {
		t.Fatalf("round trip = %+v, %v", spec, err)
	}

	(&Spec{}).Apply(app)
	if _, present := app["shmSize"]; present {
		t.Fatalf("empty spec kept the field: %v", app)
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		512 << 20: "512MiB",
		1 << 30:   "1GiB",
		4 << 10:   "4KiB",
		512:       "512B",
		3 << 29:   "1536MiB",
	}
	for bytes, want := range cases {
		if got := FormatSize(bytes); got != want {
			t.Fatalf("FormatSize(%d) = %s, want %s", bytes, got, want)
		}
	}
}
