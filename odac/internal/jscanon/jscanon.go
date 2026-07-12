// Package jscanon emits "V8-canonical JSON": byte-for-byte what Node's
// JSON.stringify would produce for the same value. The Hub protocol signs
// messages over JSON.stringify output (contract 0.2, "3.6 DECISION"), and the
// Hub verifies our outbound messages by re-stringifying ITS parse of our
// bytes — so everything we sign must be a JSON.stringify∘JSON.parse fixed
// point. That means:
//
//   - compact output (no whitespace);
//   - V8 property order: array-index-like keys first in ascending numeric
//     order, then the remaining keys in insertion order (Obj preserves the
//     caller's order; plain Go maps have no insertion order, so their
//     non-index keys are emitted sorted — any fixed order is a valid
//     "insertion order" and survives the Hub's parse→stringify round trip);
//   - V8 minimal string escaping: only `"`, `\` and control chars, with the
//     \b \t \n \f \r shortcuts and lowercase \u00xx for the rest — NO HTML
//     escaping (encoding/json would emit <), raw U+2028/U+2029;
//   - ECMA-262 Number::toString(10) formatting: integers plain up to 21
//     digits, shortest round-trip decimals, exponent form only for
//     |x| >= 1e21 or < 1e-6, and -0 renders as "0". NaN/±Inf become null
//     like JSON.stringify.
//
// Canon re-canonicalizes existing JSON text (e.g. json.RawMessage produced
// by encoding/json, whose number/escape forms differ): it decodes the token
// stream preserving object key order and re-emits through the same rules,
// which is exactly V8's JSON.parse→JSON.stringify transformation. Known
// limitation (documented in the contract): JSON text containing unpaired
// surrogate escapes cannot round-trip through Go strings; the only
// legitimate peer is the Node Hub, which never emits them.
package jscanon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Field is one ordered object member.
type Field struct {
	K string
	V any
}

// Obj is an insertion-ordered JSON object. Keys are assumed unique; when
// building from parsed JSON use Canon/decodeTree, which dedupes last-wins
// like a JS object literal.
type Obj []Field

// Marshal renders v as V8-canonical JSON.
func Marshal(v any) ([]byte, error) {
	return Append(nil, v)
}

// Append appends the V8-canonical JSON encoding of v to dst.
//
// Supported values: nil, bool, string, numeric types, json.Number,
// json.RawMessage (re-canonicalized), Obj, map[string]any, []any and
// slices of the former.
func Append(dst []byte, v any) ([]byte, error) {
	switch val := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		if val {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case string:
		return appendString(dst, val), nil
	case float64:
		return appendNumber(dst, val), nil
	case float32:
		return appendNumber(dst, float64(val)), nil
	case int:
		return appendNumber(dst, float64(val)), nil
	case int32:
		return appendNumber(dst, float64(val)), nil
	case int64:
		return appendNumber(dst, float64(val)), nil
	case uint:
		return appendNumber(dst, float64(val)), nil
	case uint32:
		return appendNumber(dst, float64(val)), nil
	case uint64:
		return appendNumber(dst, float64(val)), nil
	case json.Number:
		// JSON.parse would have produced a float64 here; formatting that
		// float64 is exactly V8's parse→stringify number transformation
		// (9007199254740993 → 9007199254740992, 1e21 → 1e+21, …).
		f, err := strconv.ParseFloat(string(val), 64)
		if err != nil {
			return dst, fmt.Errorf("jscanon: bad number literal %q", val)
		}
		return appendNumber(dst, f), nil
	case json.RawMessage:
		return appendRaw(dst, val)
	case Obj:
		return appendObj(dst, val)
	case map[string]any:
		return appendMap(dst, val)
	case []any:
		return appendArray(dst, val)
	default:
		return dst, fmt.Errorf("jscanon: unsupported type %T", v)
	}
}

// Canon re-canonicalizes raw JSON text into V8-canonical form.
func Canon(raw []byte) ([]byte, error) {
	return appendRaw(nil, raw)
}

func appendArray(dst []byte, list []any) ([]byte, error) {
	dst = append(dst, '[')
	var err error
	for i, item := range list {
		if i > 0 {
			dst = append(dst, ',')
		}
		if dst, err = Append(dst, item); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// appendObj emits an ordered object applying V8's property-order rule:
// array-index-like keys first (ascending numeric), then the rest in the
// given order.
func appendObj(dst []byte, fields Obj) ([]byte, error) {
	indexKeys := make([]Field, 0)
	rest := make([]Field, 0, len(fields))
	for _, f := range fields {
		if isArrayIndex(f.K) {
			indexKeys = append(indexKeys, f)
		} else {
			rest = append(rest, f)
		}
	}
	sort.SliceStable(indexKeys, func(i, j int) bool {
		a, _ := strconv.ParseUint(indexKeys[i].K, 10, 64)
		b, _ := strconv.ParseUint(indexKeys[j].K, 10, 64)
		return a < b
	})

	dst = append(dst, '{')
	var err error
	first := true
	for _, f := range append(indexKeys, rest...) {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = appendString(dst, f.K)
		dst = append(dst, ':')
		if dst, err = Append(dst, f.V); err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

func appendMap(dst []byte, m map[string]any) ([]byte, error) {
	fields := make(Obj, 0, len(m))
	for k := range m {
		fields = append(fields, Field{K: k})
	}
	// Non-index keys sorted (see the package comment); index keys are
	// reordered by appendObj anyway.
	sort.Slice(fields, func(i, j int) bool { return fields[i].K < fields[j].K })
	for i := range fields {
		fields[i].V = m[fields[i].K]
	}
	return appendObj(dst, fields)
}

// isArrayIndex reports whether key is a canonical array index per V8:
// "0" or a no-leading-zero decimal < 4294967295 (2^32-1 itself is NOT one).
func isArrayIndex(key string) bool {
	if key == "" || len(key) > 10 {
		return false
	}
	if key == "0" {
		return true
	}
	if key[0] == '0' {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(key, 10, 64)
	return err == nil && n < 4294967295
}

const hexDigits = "0123456789abcdef"

// appendString writes a JSON string with V8's minimal escaping.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x20 && b != '"' && b != '\\' {
			continue // multi-byte UTF-8 passes through raw
		}
		dst = append(dst, s[start:i]...)
		switch b {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func appendNumber(dst []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(dst, "null"...) // JSON.stringify(NaN|±Infinity) → null
	}
	return append(dst, NumberToString(f)...)
}

// NumberToString implements ECMA-262 Number::toString(radix 10) for finite
// values — the exact formatting V8 uses inside JSON.stringify.
func NumberToString(f float64) string {
	if f == 0 {
		return "0" // includes -0
	}
	neg := math.Signbit(f)
	if neg {
		f = -f
	}

	// Shortest round-trip digits via strconv: "d[.ddd]e±xx".
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expPart, _ := strings.Cut(sci, "e")
	exp10, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n := exp10 + 1 // decimal point position: value = 0.digits * 10^n

	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		e := n - 1
		sign := "+"
		if e < 0 {
			sign = "-"
			e = -e
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(e)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(e)
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

// appendRaw re-canonicalizes raw JSON text (see Canon).
func appendRaw(dst []byte, raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tree, err := decodeTree(dec)
	if err != nil {
		return dst, err
	}
	// Reject trailing garbage so a truncated splice can't sign quietly.
	if dec.More() {
		return dst, errors.New("jscanon: trailing data after JSON value")
	}
	return Append(dst, tree)
}

// decodeTree consumes one JSON value from dec into an order-preserving tree
// (Obj for objects, []any for arrays, json.Number for numbers). Duplicate
// object keys dedupe last-wins, like JSON.parse.
func decodeTree(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeTreeToken(dec, tok)
}

func decodeTreeToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := Obj{}
			seen := map[string]int{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("jscanon: non-string object key")
				}
				val, err := decodeTree(dec)
				if err != nil {
					return nil, err
				}
				if idx, dup := seen[key]; dup {
					obj[idx].V = val // last wins, position kept (V8 semantics)
				} else {
					seen[key] = len(obj)
					obj = append(obj, Field{K: key, V: val})
				}
			}
			_, err := dec.Token() // consume '}'
			return obj, err
		case '[':
			list := []any{}
			for dec.More() {
				val, err := decodeTree(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, val)
			}
			_, err := dec.Token() // consume ']'
			return list, err
		}
		return nil, fmt.Errorf("jscanon: unexpected delimiter %v", t)
	default:
		return tok, nil // string, json.Number, bool, nil
	}
}
