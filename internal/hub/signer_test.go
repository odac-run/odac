package hub

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"odac/internal/jscanon"
)

// signingFixtures is the Node-generated corpus (testdata/gen-fixtures.mjs
// running the REAL MessageSigner.sign) — the differential harness required
// by hub-protocol.md's "3.6 DECISION".
type signingFixtures struct {
	Sign []struct {
		Name             string `json:"name"`
		Secret           string `json:"secret"`
		MessageJSON      string `json:"message_json"`
		PayloadCanonical string `json:"payload_canonical"`
		Signature        string `json:"signature"`
	} `json:"sign"`
	Verify []struct {
		Name      string `json:"name"`
		Secret    string `json:"secret"`
		Wire      string `json:"wire"`
		Timestamp int64  `json:"timestamp"`
	} `json:"verify"`
}

func loadSigningFixtures(t *testing.T) signingFixtures {
	t.Helper()
	raw, err := os.ReadFile("testdata/signing-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f signingFixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestSignAgainstNodeGolden replays the corpus through the outbound path:
// the message fields are decoded from the Node-stringified message and
// re-signed via jscanon; canonical payload bytes AND signature must match
// MessageSigner.sign byte-for-byte.
func TestSignAgainstNodeGolden(t *testing.T) {
	for _, fx := range loadSigningFixtures(t).Sign {
		fields, err := spliceTopLevel([]byte(fx.MessageJSON))
		if err != nil {
			t.Fatalf("%s: splice: %v", fx.Name, err)
		}

		payload := jscanon.Obj{}
		if idRaw, ok := fields["id"]; ok && rawTruthy(idRaw) {
			payload = append(payload, jscanon.Field{K: "id", V: json.RawMessage(idRaw)})
		}
		payload = append(payload,
			jscanon.Field{K: "type", V: json.RawMessage(fields["type"])},
			jscanon.Field{K: "data", V: json.RawMessage(fields["data"])},
			jscanon.Field{K: "timestamp", V: json.RawMessage(fields["timestamp"])},
		)
		canonical, err := jscanon.Marshal(payload)
		if err != nil {
			t.Fatalf("%s: canon: %v", fx.Name, err)
		}
		if string(canonical) != fx.PayloadCanonical {
			t.Errorf("%s: canonical payload mismatch\n got %s\nwant %s", fx.Name, canonical, fx.PayloadCanonical)
			continue
		}
		if got := hmacHex(fx.Secret, canonical); got != fx.Signature {
			t.Errorf("%s: signature mismatch: %s != %s", fx.Name, got, fx.Signature)
		}
	}
}

// TestVerifyWireAgainstNodeGolden feeds the hostile-key-order wire fixtures
// through the raw-splice verifier.
func TestVerifyWireAgainstNodeGolden(t *testing.T) {
	for _, fx := range loadSigningFixtures(t).Verify {
		now := time.Unix(fx.Timestamp, 0)
		if ok, reason := verifyWire([]byte(fx.Wire), fx.Secret, now); !ok {
			t.Errorf("%s: verify failed: %s", fx.Name, reason)
		}
		// A shifted clock outside the 300s window must reject the same wire.
		if ok, _ := verifyWire([]byte(fx.Wire), fx.Secret, now.Add(301*time.Second)); ok {
			t.Errorf("%s: stale message verified", fx.Name)
		}
	}
}

func TestVerifyWireRejections(t *testing.T) {
	const secret = "s3cret"
	now := time.Unix(1752300000, 0)
	ts := now.Unix()

	signed := func(id any, msgType string, data any, timestamp int64) []byte {
		sig, err := sign(id, msgType, data, timestamp, secret)
		if err != nil {
			t.Fatal(err)
		}
		msg := jscanon.Obj{}
		if id != nil {
			msg = append(msg, jscanon.Field{K: "id", V: id})
		}
		msg = append(msg,
			jscanon.Field{K: "type", V: msgType},
			jscanon.Field{K: "data", V: data},
			jscanon.Field{K: "timestamp", V: timestamp},
			jscanon.Field{K: "signature", V: sig},
		)
		raw, err := jscanon.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	valid := signed(nil, "command", map[string]any{"action": "app.list"}, ts)
	if ok, reason := verifyWire(valid, secret, now); !ok {
		t.Fatalf("valid message rejected: %s", reason)
	}

	cases := []struct {
		name string
		wire []byte
		now  time.Time
	}{
		{"wrong secret", valid, now}, // checked separately below
		{"missing signature", []byte(`{"type":"x","data":null,"timestamp":` + jscanon.NumberToString(float64(ts)) + `}`), now},
		{"missing timestamp", []byte(`{"type":"x","data":null,"signature":"ab"}`), now},
		{"too old", signed(nil, "x", nil, ts-301), now},
		{"in future", signed(nil, "x", nil, ts+301), now},
		{"tampered", tamper(valid), now},
		{"not json", []byte(`hello`), now},
		{"array", []byte(`[1,2]`), now},
	}
	for _, tc := range cases {
		sec := secret
		if tc.name == "wrong secret" {
			sec = "other"
		}
		if ok, _ := verifyWire(tc.wire, sec, tc.now); ok {
			t.Errorf("%s: verify unexpectedly passed", tc.name)
		}
	}

	// Boundary: exactly 300s old still verifies (Node: > maxAge rejects).
	if ok, reason := verifyWire(signed(nil, "x", nil, ts-300), secret, now); !ok {
		t.Errorf("300s-old message rejected: %s", reason)
	}

	// A falsy id ("" / 0) is EXCLUDED from the signed payload; a message
	// whose signature was computed without id but which carries id:"" on
	// the wire must verify.
	sig, _ := sign(nil, "x", nil, ts, secret)
	wire := []byte(`{"id":"","type":"x","data":null,"timestamp":` +
		jscanon.NumberToString(float64(ts)) + `,"signature":"` + sig + `"}`)
	if ok, reason := verifyWire(wire, secret, now); !ok {
		t.Errorf("falsy-id message rejected: %s", reason)
	}

	// No secret configured: every signed message fails (Node: sign()
	// returns null, comparison against a real signature never matches).
	if ok, _ := verifyWire(valid, "", now); ok {
		t.Error("message verified with empty secret")
	}
}

// tamper flips the final hex digit of the trailing signature value.
func tamper(wire []byte) []byte {
	out := append([]byte(nil), wire...)
	i := len(out) - 3 // …<hex>"}
	if out[i] == '0' {
		out[i] = '1'
	} else {
		out[i] = '0'
	}
	return out
}

func TestSignNoSecret(t *testing.T) {
	sig, err := sign(nil, "x", nil, 1, "")
	if err != nil || sig != "" {
		t.Fatalf("sign without secret = %q, %v; want empty", sig, err)
	}
}

func TestJSTruthy(t *testing.T) {
	truthy := []any{true, "x", float64(1), -1, map[string]any{}, []any{}}
	falsy := []any{nil, false, "", float64(0), 0}
	for _, v := range truthy {
		if !jsTruthy(v) {
			t.Errorf("jsTruthy(%#v) = false", v)
		}
	}
	for _, v := range falsy {
		if jsTruthy(v) {
			t.Errorf("jsTruthy(%#v) = true", v)
		}
	}
}
