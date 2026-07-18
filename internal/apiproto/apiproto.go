// Package apiproto implements the client side of ODAC's local API socket
// protocol (docs/migration/contracts/api-protocol.md): one JSON request per
// TCP connection to 127.0.0.1:1453, answered by zero or more \r\n-terminated
// progress lines followed by exactly one final response (no trailing newline),
// after which the server destroys the connection.
package apiproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultAddr is the TCP address the server listens on. The CLI only ever
// uses TCP (never the unix socket) — see "Client behavior" in the contract.
const DefaultAddr = "127.0.0.1:1453"

// DefaultDialTimeout mirrors the 2s liveness-check timeout in Connector.js.
const DefaultDialTimeout = 2 * time.Second

// Request is the single JSON document sent per connection. Data is spread by
// the server as positional arguments into the command handler.
type Request struct {
	Auth   string `json:"auth"`
	Action string `json:"action"`
	Data   []any  `json:"data"`
}

// Progress is one streamed progress line (identified by its "status" key).
type Progress struct {
	Message string `json:"message"`
	Process string `json:"process"`
	Status  string `json:"status"`
}

// Response is the final response (identified by its "result" key). Message is
// "" when the server sent null; Data is nil when the server omitted it. The
// `invalid_json` error response arrives without an ID.
type Response struct {
	Data    json.RawMessage `json:"data"`
	ID      string          `json:"id"`
	Message string          `json:"message"`
	Result  bool            `json:"result"`
}

// Client calls the local API server.
type Client struct {
	// Addr is the TCP address; DefaultAddr when empty.
	Addr string
	// DialTimeout bounds connection establishment only (calls themselves
	// have no deadline: actions may stream progress for minutes, matching
	// Node's un-timed socket). DefaultDialTimeout when zero.
	DialTimeout time.Duration
}

func (c *Client) addr() string {
	if c.Addr != "" {
		return c.Addr
	}
	return DefaultAddr
}

func (c *Client) dialTimeout() time.Duration {
	if c.DialTimeout != 0 {
		return c.DialTimeout
	}
	return DefaultDialTimeout
}

// Call sends req in a single write and reads the reply stream until the final
// response. onProgress, when non-nil, is invoked for each progress line in
// arrival order. A final response with Result=false is returned without error;
// errors mean the call itself failed (connect, write, connection closed early).
func (c *Client) Call(req Request, onProgress func(Progress)) (*Response, error) {
	conn, err := net.DialTimeout("tcp", c.addr(), c.dialTimeout())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if req.Data == nil {
		req.Data = []any{}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Single write: the server parses each socket read as one complete JSON
	// document (no length framing — see "Framing caveats" in the contract).
	if _, err := conn.Write(payload); err != nil {
		return nil, err
	}

	return readReply(conn, onProgress)
}

// readReply consumes the reply stream. Progress lines are \r\n-terminated and
// may coalesce with each other and with the final response in one segment;
// the final response is the only line without a terminator, so the buffered
// tail is probed after every read (this also tolerates a fragmented final,
// which Node's client would drop — deliberately more lenient, per contract).
func readReply(conn net.Conn, onProgress func(Progress)) (*Response, error) {
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.Index(buf, []byte("\r\n"))
				if i < 0 {
					break
				}
				line := buf[:i]
				buf = buf[i+2:]
				if resp, ok := parseFinal(line); ok {
					return resp, nil
				}
				if p, ok := parseProgress(line); ok && onProgress != nil {
					onProgress(p)
				}
			}
			if resp, ok := parseFinal(buf); ok {
				return resp, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if resp, ok := parseFinal(buf); ok {
					return resp, nil
				}
				return nil, fmt.Errorf("apiproto: connection closed before final response")
			}
			return nil, err
		}
	}
}

// parseFinal reports whether raw is the final response (has a "result" key).
func parseFinal(raw []byte) (*Response, bool) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return nil, false
	}
	if _, ok := probe["result"]; !ok {
		return nil, false
	}
	var resp Response
	if json.Unmarshal(raw, &resp) != nil {
		return nil, false
	}
	return &resp, true
}

// parseProgress reports whether raw is a progress line (has a "status" key).
func parseProgress(raw []byte) (Progress, bool) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return Progress{}, false
	}
	if _, ok := probe["status"]; !ok {
		return Progress{}, false
	}
	var p Progress
	if json.Unmarshal(raw, &p) != nil {
		return Progress{}, false
	}
	return p, true
}

// Ping reports whether the server accepts TCP connections on addr — the
// primary liveness probe (works in Docker where PIDs aren't visible).
func Ping(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
