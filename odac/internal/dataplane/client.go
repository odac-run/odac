// Package dataplane holds the orchestrator-side services for the Go
// data-plane binaries — the ports of server/src/{Proxy,DNS,Mail}.js on top of
// internal/supervise. Each service implements its system.Services slot
// (Start/Stop/Check) and pushes full-replace config over the binary's unix
// control socket per contracts/{proxy,dns,mail}-control.md.
//
// Task 3.2 decision (recorded in STATE.md): the TCP fallback mode of the
// control APIs is NOT ported. It was dead code in Node — the binaries printed
// their port to a stdout the orchestrator discards — so the socket env var is
// simply always set, on every platform.
package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// process is the supervisor surface the services drive;
// *supervise.Supervisor implements it (tests substitute fakes).
type process interface {
	Ensure()
	Stop()
	Running() bool
	SocketPath() string
}

// syncRetries is Node's retry cap for config pushes (retryCount < 3).
const syncRetries = 3

func socketClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

// postJSON POSTs payload to the module's control API. Any HTTP status is
// accepted (Node: validateStatus () => true, response ignored); only
// transport errors are reported.
func postJSON(socketPath, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := socketClient(socketPath, 0).Post("http://localhost"+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// getStatus GETs path and returns the HTTP status code.
func getStatus(socketPath, path string, timeout time.Duration) (int, error) {
	resp, err := socketClient(socketPath, timeout).Get("http://localhost" + path)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// retryable mirrors Node's retry set for config sync: ECONNREFUSED (binary
// not accepting yet), ENOENT (socket file gone), ECONNRESET.
func retryable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNRESET)
}

// errCode names the transport error like Node's e.code, for log parity.
func errCode(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.ENOENT):
		return "ENOENT"
	case errors.Is(err, syscall.ECONNRESET):
		return "ECONNRESET"
	}
	return err.Error()
}
