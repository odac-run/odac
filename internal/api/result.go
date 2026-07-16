package api

import (
	"encoding/json"
	"reflect"
)

// Result is the server-side response envelope, the port of Api.result() in
// server/src/Api.js. Use Res to construct it — the constructor reproduces
// Node's normalization: when data is omitted and message is an object, the
// message moves to data and message becomes an explicit JSON null.
type Result struct {
	Status bool
	// Message is nil for JS undefined (key omitted on the wire); NullMsg
	// marks the moved-object case where Node emits an explicit null.
	Message any
	NullMsg bool
	Data    any
	HasData bool
}

// Res ports result(status, message, data): pass zero or one data value.
func Res(status bool, message any, data ...any) Result {
	r := Result{Status: status}
	if len(data) > 0 {
		r.Data, r.HasData = data[0], true
		r.Message = message
		return r
	}
	if isJSObject(message) {
		r.Data, r.HasData = message, true
		r.NullMsg = true
		return r
	}
	r.Message = message
	return r
}

// isJSObject mirrors `typeof v === 'object' && v !== null`: maps, slices and
// other composites qualify; scalars and nil do not.
func isJSObject(v any) bool {
	switch v.(type) {
	case nil, string, bool, float64, int, int64:
		return false
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer:
		return true
	}
	return false
}

// encodeFinal builds the final response bytes: JSON.stringify({id, ...result})
// with Node's key order (id, result, message, data) and its undefined-key
// omission. A nil result reproduces the server.stop quirk — the handler
// returned undefined, so the response is just {"id":"..."}. withID=false
// drops the id field (only invalid_json is sent that way).
func encodeFinal(id string, withID bool, r *Result) []byte {
	out := []byte{'{'}
	if withID {
		out = appendField(out, "id", id)
	}
	if r != nil {
		out = appendField(out, "result", r.Status)
		if r.Message != nil || r.NullMsg {
			out = appendField(out, "message", r.Message)
		}
		if r.HasData {
			out = appendField(out, "data", r.Data)
		}
	}
	return append(out, '}')
}

// encodeProgress builds one streamed progress line (Api.send): key order
// process, status, message; nil values are omitted like JS undefined.
func encodeProgress(process, status, message any) []byte {
	out := []byte{'{'}
	if process != nil {
		out = appendField(out, "process", process)
	}
	if status != nil {
		out = appendField(out, "status", status)
	}
	if message != nil {
		out = appendField(out, "message", message)
	}
	return append(append(out, '}'), '\r', '\n')
}

func appendField(out []byte, key string, v any) []byte {
	if len(out) > 1 {
		out = append(out, ',')
	}
	out = append(out, '"')
	out = append(out, key...)
	out = append(out, '"', ':')
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte("null")
	}
	return append(out, raw...)
}
