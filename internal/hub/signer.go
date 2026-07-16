// signer.go ports MessageSigner (server/src/Hub/WebSocket.js) per the
// "3.6 DECISION" in contracts/hub-protocol.md:
//
//   - Outbound sign: the payload object {id?, type, data, timestamp} is
//     emitted as V8-canonical JSON (internal/jscanon) and HMAC-SHA256'd.
//   - Inbound verify: the raw wire bytes are SPLICED, not parsed and
//     re-stringified — the value slices of id/type/data/timestamp are
//     extracted byte-for-byte (only inter-token whitespace dropped) and
//     concatenated in the mandated key order. Nothing is re-encoded, so
//     V8's number/escape forms survive untouched.
package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"odac/internal/jscanon"
)

// maxMessageAge mirrors MessageSigner.verify's 300s window.
const maxMessageAge = 300

// sign computes the hex HMAC-SHA256 signature over the canonical payload.
// Mirrors MessageSigner.sign: a missing secret yields no signature (the
// caller emits null), and id is included only when truthy.
func sign(id any, msgType string, data any, timestamp int64, secret string) (string, error) {
	if secret == "" {
		return "", nil
	}
	payload := jscanon.Obj{}
	if jsTruthy(id) {
		payload = append(payload, jscanon.Field{K: "id", V: id})
	}
	payload = append(payload,
		jscanon.Field{K: "type", V: msgType},
		jscanon.Field{K: "data", V: data},
		jscanon.Field{K: "timestamp", V: timestamp},
	)
	canonical, err := jscanon.Marshal(payload)
	if err != nil {
		return "", err
	}
	return hmacHex(secret, canonical), nil
}

func hmacHex(secret string, msg []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(msg)
	return hex.EncodeToString(mac.Sum(nil))
}

// jsTruthy mirrors JavaScript truthiness for decoded-JSON values.
func jsTruthy(v any) bool {
	switch val := v.(type) {
	case nil:
		return false
	case bool:
		return val
	case string:
		return val != ""
	case float64:
		return val != 0 && !math.IsNaN(val)
	case int:
		return val != 0
	case int64:
		return val != 0
	}
	return true // objects and arrays are always truthy
}

// verifyWire ports MessageSigner.verify over raw wire bytes. It returns
// false with a log-worthy reason when the message must be dropped.
func verifyWire(raw []byte, secret string, now time.Time) (ok bool, reason string) {
	fields, err := spliceTopLevel(raw)
	if err != nil {
		return false, "unparseable message: " + err.Error()
	}

	sigRaw, hasSig := fields["signature"]
	tsRaw, hasTS := fields["timestamp"]
	// Node: `if (!signature || !timestamp)` — truthiness, so an empty or
	// null signature and a zero timestamp fail the same way.
	if !hasSig || !hasTS || !rawTruthy(sigRaw) || !rawTruthy(tsRaw) {
		return false, "missing signature or timestamp"
	}

	var signature string
	if json.Unmarshal(sigRaw, &signature) != nil {
		return false, "missing signature or timestamp"
	}
	ts, err := strconv.ParseFloat(string(tsRaw), 64)
	if err != nil {
		return false, "missing signature or timestamp"
	}

	if math.Abs(float64(now.Unix())-ts) > maxMessageAge {
		return false, "timestamp too old or in future"
	}

	// Reassemble the payload in the mandated order from the raw slices.
	payload := []byte{'{'}
	appendPart := func(key string, val []byte) {
		if len(payload) > 1 {
			payload = append(payload, ',')
		}
		payload = append(payload, '"')
		payload = append(payload, key...)
		payload = append(payload, '"', ':')
		payload = append(payload, val...)
	}
	if idRaw, hasID := fields["id"]; hasID && rawTruthy(idRaw) {
		appendPart("id", idRaw)
	}
	// type/data are assigned unconditionally in Node's sign(); absent keys
	// are undefined and vanish from JSON.stringify output.
	if typeRaw, hasType := fields["type"]; hasType {
		appendPart("type", typeRaw)
	}
	if dataRaw, hasData := fields["data"]; hasData {
		appendPart("data", dataRaw)
	}
	appendPart("timestamp", tsRaw)
	payload = append(payload, '}')

	expected := hmacHex(secret, payload)
	// Node compares with ===; constant-time is the contract-sanctioned
	// improvement used across the migration.
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return false, "invalid signature"
	}
	return true, ""
}

// rawTruthy reports JS truthiness of a compacted raw JSON value.
func rawTruthy(raw []byte) bool {
	switch string(raw) {
	case "null", "false", `""`:
		return false
	}
	// Any numeric zero form (0, 0.0, -0, 0e5…) is falsy.
	if f, err := strconv.ParseFloat(string(raw), 64); err == nil {
		return f != 0
	}
	return true
}

// spliceTopLevel scans one top-level JSON object and returns the COMPACTED
// raw value bytes per key (duplicate keys: last wins, like JSON.parse).
func spliceTopLevel(raw []byte) (map[string][]byte, error) {
	s := &scanner{buf: raw}
	s.skipSpace()
	if !s.expect('{') {
		return nil, errors.New("not a JSON object")
	}
	fields := map[string][]byte{}
	s.skipSpace()
	if s.peek() == '}' {
		s.pos++
		return fields, s.expectEnd()
	}
	for {
		s.skipSpace()
		keyRaw, err := s.scanString()
		if err != nil {
			return nil, err
		}
		var key string
		if json.Unmarshal(keyRaw, &key) != nil {
			return nil, errors.New("bad object key")
		}
		s.skipSpace()
		if !s.expect(':') {
			return nil, errors.New("missing colon")
		}
		val, err := s.scanValue()
		if err != nil {
			return nil, err
		}
		fields[key] = val
		s.skipSpace()
		switch s.peek() {
		case ',':
			s.pos++
		case '}':
			s.pos++
			return fields, s.expectEnd()
		default:
			return nil, errors.New("malformed object")
		}
	}
}

// scanner is the minimal JSON tokenizer behind spliceTopLevel. scanValue
// copies token bytes verbatim and drops only inter-token whitespace, per
// the contract's splice rule.
type scanner struct {
	buf []byte
	pos int
}

func (s *scanner) peek() byte {
	if s.pos >= len(s.buf) {
		return 0
	}
	return s.buf[s.pos]
}

func (s *scanner) expect(c byte) bool {
	if s.peek() != c {
		return false
	}
	s.pos++
	return true
}

func (s *scanner) skipSpace() {
	for s.pos < len(s.buf) {
		switch s.buf[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *scanner) expectEnd() error {
	s.skipSpace()
	if s.pos != len(s.buf) {
		return errors.New("trailing data")
	}
	return nil
}

// scanString consumes a JSON string token and returns its raw bytes
// (including quotes) untouched.
func (s *scanner) scanString() ([]byte, error) {
	start := s.pos
	if !s.expect('"') {
		return nil, errors.New("expected string")
	}
	for s.pos < len(s.buf) {
		switch s.buf[s.pos] {
		case '\\':
			s.pos += 2
		case '"':
			s.pos++
			return s.buf[start:s.pos], nil
		default:
			s.pos++
		}
	}
	return nil, errors.New("unterminated string")
}

// scanValue consumes one JSON value and returns its compacted raw bytes.
func (s *scanner) scanValue() ([]byte, error) {
	s.skipSpace()
	switch c := s.peek(); c {
	case '"':
		return s.scanString()
	case '{', '[':
		open, close := byte('{'), byte('}')
		if c == '[' {
			open, close = '[', ']'
		}
		out := []byte{open}
		s.pos++
		s.skipSpace()
		if s.peek() == close {
			s.pos++
			return append(out, close), nil
		}
		for {
			s.skipSpace()
			if open == '{' {
				key, err := s.scanString()
				if err != nil {
					return nil, err
				}
				out = append(out, key...)
				s.skipSpace()
				if !s.expect(':') {
					return nil, errors.New("missing colon")
				}
				out = append(out, ':')
			}
			inner, err := s.scanValue()
			if err != nil {
				return nil, err
			}
			out = append(out, inner...)
			s.skipSpace()
			switch s.peek() {
			case ',':
				s.pos++
				out = append(out, ',')
			case close:
				s.pos++
				return append(out, close), nil
			default:
				return nil, fmt.Errorf("malformed %c…%c value", open, close)
			}
		}
	case 0:
		return nil, errors.New("unexpected end of input")
	default:
		// number / true / false / null: read until a delimiter.
		start := s.pos
		for s.pos < len(s.buf) {
			switch s.buf[s.pos] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				goto done
			}
			s.pos++
		}
	done:
		if s.pos == start {
			return nil, errors.New("empty value")
		}
		return s.buf[start:s.pos], nil
	}
}
