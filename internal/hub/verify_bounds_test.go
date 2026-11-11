package hub

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// TestEmptySecretVerifiesNothing: an empty HMAC key is still a key, so a
// message signed with it used to pass whenever hub.secret was empty.
func TestEmptySecretVerifiesNothing(t *testing.T) {
	now := time.Now()
	ts := now.Unix()
	payload := `{"type":"command","data":{"action":"app.list"},"timestamp":` + strconv.FormatInt(ts, 10) + `}`
	sig := hmacHex("", []byte(payload))
	wire := []byte(payload[:len(payload)-1] + `,"signature":"` + sig + `"}`)
	if ok, _ := verifyWire(wire, "", now); ok {
		t.Fatal("a message signed with an empty key verified")
	}
}

// TestDeepNestingIsRefusedBeforeTheStackRunsOut: the splice scanner runs on
// unverified input up to the websocket's 16 MiB read limit, and the old
// recursion died with a fatal (unrecoverable) stack overflow on it.
func TestDeepNestingIsRefusedBeforeTheStackRunsOut(t *testing.T) {
	for _, open := range []string{"[", `{"a":`} {
		n := 16 * 1024 * 1024 / len(open)
		raw := append([]byte(`{"data":`), bytes.Repeat([]byte(open), n)...)
		if ok, reason := verifyWire(raw, "secret", time.Now()); ok || reason == "" {
			t.Fatalf("%q x %d: ok=%v reason=%q", open, n, ok, reason)
		}
	}
}

// TestNestingWithinTheDecoderLimitStillSplices keeps every message the
// handler's json.Unmarshal accepts verifiable.
func TestNestingWithinTheDecoderLimitStillSplices(t *testing.T) {
	depth := maxNestingDepth - 1 // the top-level object is one more level
	data := bytes.Repeat([]byte("["), depth)
	data = append(data, bytes.Repeat([]byte("]"), depth)...)
	raw := append(append([]byte(`{"data":`), data...), '}')
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoder refused depth %d: %v", depth, err)
	}
	if _, err := spliceTopLevel(raw); err != nil {
		t.Fatalf("splice refused what the decoder accepts: %v", err)
	}
}
