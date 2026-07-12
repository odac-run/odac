package jscanon

import (
	"encoding/json"
	"os"
	"testing"
)

// fixtures is the Node-generated corpus (internal/hub/testdata, produced by
// gen-fixtures.mjs running the real MessageSigner/JSON.stringify).
type fixtures struct {
	Sign []struct {
		Name             string `json:"name"`
		MessageJSON      string `json:"message_json"`
		PayloadCanonical string `json:"payload_canonical"`
	} `json:"sign"`
	Numbers map[string]string `json:"numbers"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	raw, err := os.ReadFile("../hub/testdata/signing-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestNumberFormattingGolden replays V8's number table: for every literal,
// Canon must produce exactly JSON.stringify(JSON.parse(literal)).
func TestNumberFormattingGolden(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Numbers) == 0 {
		t.Fatal("no number fixtures")
	}
	for literal, want := range f.Numbers {
		got, err := Canon([]byte(literal))
		if err != nil {
			t.Errorf("Canon(%q): %v", literal, err)
			continue
		}
		if string(got) != want {
			t.Errorf("Canon(%q) = %s, want %s", literal, got, want)
		}
	}
}

// TestCanonFixedPoint: Node's own stringify output must pass through Canon
// unchanged (the parse→stringify fixed-point property the signing scheme
// rests on).
func TestCanonFixedPoint(t *testing.T) {
	f := loadFixtures(t)
	for _, fx := range f.Sign {
		for _, text := range []string{fx.MessageJSON, fx.PayloadCanonical} {
			got, err := Canon([]byte(text))
			if err != nil {
				t.Errorf("%s: Canon error: %v", fx.Name, err)
				continue
			}
			if string(got) != text {
				t.Errorf("%s: Canon changed canonical text\n got %s\nwant %s", fx.Name, got, text)
			}
		}
	}
}

// TestCanonNormalizesGoJSON: encoding/json's divergent forms (HTML escapes,
// zero-padded exponents, insignificant whitespace) must canonicalize to the
// V8 form.
func TestCanonNormalizesGoJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"\u003ctag\u003e \u0026 done"`, `"<tag> & done"`}, // Go HTML escaping undone
		{`1e-07`, `1e-7`},  // Go zero-pads exponents
		{`1E+21`, `1e+21`}, // upper-case exponent
		{`1.0`, `1`},       // trailing fraction
		{`{ "b" : 1 , "a" : [ 1 , 2 ] }`, `{"b":1,"a":[1,2]}`}, // whitespace + order kept
		{`{"10":1,"x":2,"2":3}`, `{"2":3,"10":1,"x":2}`},       // V8 index-key reorder
		{`{"a":1,"a":2}`, `{"a":2}`},                           // duplicate key: last wins
		{`"A\u00e7"`, `"Aç"`},                                  // needless escapes decoded
		{`"\u2028\u2029"`, "\"  \""},                           // JS line separators stay raw
	}
	for _, tc := range cases {
		got, err := Canon([]byte(tc.in))
		if err != nil {
			t.Errorf("Canon(%q): %v", tc.in, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("Canon(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if _, err := Canon([]byte(`{"a":1} trailing`)); err == nil {
		t.Error("Canon accepted trailing garbage")
	}
}

// TestMarshalValues covers native Go values: V8 string escaping, Obj order
// rules, map key sorting, nested composites.
func TestMarshalValues(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"null", nil, `null`},
		{"bool", true, `true`},
		{"int", 42, `42`},
		{"negzero", float64(0) * -1, `0`},
		{"string-controls", "a\b\t\n\f\r\"\\\u0001z", `"a\b\t\n\f\r\"\\\u0001z"`},
		{"string-unicode", "çğü 中文 🙂 \u2028\u2029 <>&", "\"çğü 中文 🙂 \u2028\u2029 <>&\""},
		{"array", []any{1, "x", nil, false}, `[1,"x",null,false]`},
		{"obj-order", Obj{{K: "z", V: 1}, {K: "a", V: 2}}, `{"z":1,"a":2}`},
		{"obj-index-keys", Obj{{K: "10", V: "a"}, {K: "name", V: "x"}, {K: "2", V: "b"}, {K: "01", V: "y"}, {K: "4294967295", V: "w"}},
			`{"2":"b","10":"a","name":"x","01":"y","4294967295":"w"}`},
		{"map-sorted", map[string]any{"b": 1, "a": 2, "10": 3}, `{"10":3,"a":2,"b":1}`},
		{"raw", json.RawMessage(`{"k":1e-07}`), `{"k":1e-7}`},
		{"json-number", json.Number("9007199254740993"), `9007199254740992`},
	}
	for _, tc := range cases {
		got, err := Marshal(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, got, tc.want)
		}
	}

	if _, err := Marshal(struct{}{}); err == nil {
		t.Error("Marshal accepted an unsupported type")
	}
}

func TestNumberToStringEdges(t *testing.T) {
	// Hand-picked values whose V8 renderings are pinned by the Node-run
	// numbers golden above; this table guards the pure function directly.
	cases := map[float64]string{
		0:                      "0",
		1:                      "1",
		-1:                     "-1",
		0.1:                    "0.1",
		1e21:                   "1e+21",
		1e20:                   "100000000000000000000",
		1e-6:                   "0.000001",
		1e-7:                   "1e-7",
		9007199254740991:       "9007199254740991",
		5e-324:                 "5e-324",
		1.7976931348623157e308: "1.7976931348623157e+308",
		-42.5:                  "-42.5",
		1.5e-5:                 "0.000015",
	}
	for in, want := range cases {
		if got := NumberToString(in); got != want {
			t.Errorf("NumberToString(%v) = %s, want %s", in, got, want)
		}
	}
}
