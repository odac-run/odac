package apiproto

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeServer accepts one connection at a time, reads a single request
// (mirroring Node's one-JSON-per-data-event parsing) and hands it to handler
// together with the raw bytes and the connection for scripted replies.
func fakeServer(t *testing.T, handler func(raw []byte, conn net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64*1024)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				handler(buf[:n], c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// write sends each script entry as a separate TCP write with a small gap so
// segments arrive separately; entries joined with "" in one write test the
// coalesced path instead.
func writeScript(conn net.Conn, script []string) {
	for i, part := range script {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		conn.Write([]byte(part))
	}
}

func TestCallFraming(t *testing.T) {
	// Contract: progress lines end with \r\n and may coalesce with each other
	// and with the final response; the final response has no trailing newline
	// and is normally a single write.
	final := `{"id":"abc123","result":true,"message":"done","data":{"n":1}}`
	p1 := `{"process":"step1","status":"progress","message":"working"}` + "\r\n"
	p2 := `{"process":"step1","status":"success","message":"worked"}` + "\r\n"

	tests := []struct {
		name         string
		script       []string // each entry = one TCP write
		wantProgress []string // status values in order
	}{
		{"final only", []string{final}, nil},
		{"progress then final, separate writes", []string{p1, p2, final}, []string{"progress", "success"}},
		{"everything coalesced in one write", []string{p1 + p2 + final}, []string{"progress", "success"}},
		{"progress coalesced, final separate", []string{p1 + p2, final}, []string{"progress", "success"}},
		{"final fragmented across writes", []string{p1, final[:20], final[20:]}, []string{"progress"}},
		{"final terminated with crlf (tolerated)", []string{p1 + final + "\r\n"}, []string{"progress"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := fakeServer(t, func(raw []byte, conn net.Conn) {
				writeScript(conn, tt.script)
			})
			var got []string
			c := &Client{Addr: addr}
			resp, err := c.Call(Request{Auth: "tok", Action: "x"}, func(p Progress) {
				got = append(got, p.Status)
			})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !resp.Result || resp.ID != "abc123" || resp.Message != "done" {
				t.Errorf("final = %+v", resp)
			}
			if string(resp.Data) != `{"n":1}` {
				t.Errorf("data = %s", resp.Data)
			}
			if !reflect.DeepEqual(got, tt.wantProgress) {
				t.Errorf("progress = %v, want %v", got, tt.wantProgress)
			}
		})
	}
}

func TestCallRequestEncoding(t *testing.T) {
	reqs := make(chan []byte, 1)
	addr := fakeServer(t, func(raw []byte, conn net.Conn) {
		reqs <- raw
		conn.Write([]byte(`{"id":"x","result":true,"message":null}`))
	})

	c := &Client{Addr: addr}
	if _, err := c.Call(Request{Auth: "roottoken", Action: "domain.add", Data: []any{"example.com", 5}}, nil); err != nil {
		t.Fatal(err)
	}

	raw := <-reqs
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("server received invalid JSON: %v", err)
	}
	want := map[string]any{"auth": "roottoken", "action": "domain.add", "data": []any{"example.com", float64(5)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request = %v, want %v", got, want)
	}
}

func TestCallNilDataSentAsEmptyArray(t *testing.T) {
	reqs := make(chan []byte, 1)
	addr := fakeServer(t, func(raw []byte, conn net.Conn) {
		reqs <- raw
		conn.Write([]byte(`{"id":"x","result":true,"message":null}`))
	})

	c := &Client{Addr: addr}
	if _, err := c.Call(Request{Auth: "t", Action: "app.list"}, nil); err != nil {
		t.Fatal(err)
	}
	if raw := <-reqs; !strings.Contains(string(raw), `"data":[]`) {
		t.Errorf("nil Data should encode as [], got: %s", raw)
	}
}

func TestCallNullMessageAndMissingData(t *testing.T) {
	addr := fakeServer(t, func(raw []byte, conn net.Conn) {
		conn.Write([]byte(`{"id":"q","result":true,"message":null}`))
	})
	resp, err := (&Client{Addr: addr}).Call(Request{Auth: "t", Action: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message != "" || resp.Data != nil {
		t.Errorf("resp = %+v", resp)
	}
}

func TestCallInvalidJSONErrorResponse(t *testing.T) {
	// The invalid_json error final is sent WITHOUT an id field.
	addr := fakeServer(t, func(raw []byte, conn net.Conn) {
		conn.Write([]byte(`{"result":false,"message":"invalid_json"}`))
	})
	resp, err := (&Client{Addr: addr}).Call(Request{Auth: "t", Action: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result || resp.Message != "invalid_json" || resp.ID != "" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestCallConnectionClosedBeforeFinal(t *testing.T) {
	// Non-allowlisted IPs get their socket destroyed silently; a crash
	// mid-action looks the same. Must surface as an error, not a hang.
	addr := fakeServer(t, func(raw []byte, conn net.Conn) {
		conn.Write([]byte(`{"process":"p","status":"progress","message":"m"}` + "\r\n"))
	})
	_, err := (&Client{Addr: addr}).Call(Request{Auth: "t", Action: "x"}, nil)
	if err == nil {
		t.Fatal("want error when server closes without a final response")
	}
}

func TestPing(t *testing.T) {
	addr := fakeServer(t, func(raw []byte, conn net.Conn) {})
	if !Ping(addr, time.Second) {
		t.Error("Ping(listening addr) = false, want true")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	if Ping(dead, time.Second) {
		t.Error("Ping(closed addr) = true, want false")
	}
}
