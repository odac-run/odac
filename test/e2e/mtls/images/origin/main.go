// origin is the app the mTLS end-to-end test puts behind the ODAC proxy. It
// reports what the proxy told it about the client certificate and checks the
// proxy's HMAC over those headers itself, with the key the agent hands every
// app with a policy (ODAC_CLIENT_CERT_KEY). It never logs header values or the
// key: the harness greps every log for leaks, this one included.
//
//	GET /     JSON verdict (see report)
//	GET /ws   WebSocket that sends its verdict, then "tick N" every second
//
// `origin api <action>` (run with docker exec) calls the agent's local API
// with the app's own ODAC_API_KEY, the way an app with a grant would.
package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	hVerify      = "X-Odac-Client-Verify"
	hSubject     = "X-Odac-Client-Subject"
	hIssuer      = "X-Odac-Client-Issuer"
	hFingerprint = "X-Odac-Client-Fingerprint"
	hSerial      = "X-Odac-Client-Serial"
	hNotAfter    = "X-Odac-Client-Not-After"
	hSignature   = "X-Odac-Client-Signature"
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxSkew      = 60 // seconds, per docs/server/06-domain/04-client-certificates.md
)

type report struct {
	App         string `json:"app"`
	Method      string `json:"method"`
	Host        string `json:"host"`
	URI         string `json:"uri"`
	Verify      string `json:"verify"`
	Subject     string `json:"subject"`
	Issuer      string `json:"issuer"`
	Fingerprint string `json:"fingerprint"`
	Serial      string `json:"serial"`
	NotAfter    string `json:"notAfter"`
	SigPresent  bool   `json:"sig_present"`
	KeyPresent  bool   `json:"key_present"`
	SigValid    bool   `json:"sig_valid"`
	SigReason   string `json:"sig_reason"`
}

// verify rebuilds the signed string exactly as the docs specify and compares
// in constant time. Any failure leaves the request anonymous.
func verify(r *http.Request, key string) (bool, string) {
	sig := r.Header.Get(hSignature)
	if sig == "" {
		return false, "no_signature"
	}
	if key == "" {
		return false, "no_key"
	}
	var ts, v1 string
	for _, part := range strings.Split(sig, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || v1 == "" {
		return false, "malformed"
	}
	if d := time.Now().Unix() - t; d > maxSkew || d < -maxSkew {
		return false, "stale"
	}
	msg := strings.Join([]string{
		"v1", ts, r.Method, r.Host, r.RequestURI,
		r.Header.Get(hVerify), r.Header.Get(hSubject), r.Header.Get(hIssuer),
		r.Header.Get(hFingerprint), r.Header.Get(hSerial), r.Header.Get(hNotAfter),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(v1)) {
		return false, "mismatch"
	}
	return true, "ok"
}

func build(r *http.Request) report {
	key := os.Getenv("ODAC_CLIENT_CERT_KEY")
	ok, reason := verify(r, key)
	return report{
		App:         "mtls-e2e-origin",
		Method:      r.Method,
		Host:        r.Host,
		URI:         r.RequestURI,
		Verify:      r.Header.Get(hVerify),
		Subject:     r.Header.Get(hSubject),
		Issuer:      r.Header.Get(hIssuer),
		Fingerprint: r.Header.Get(hFingerprint),
		Serial:      r.Header.Get(hSerial),
		NotAfter:    r.Header.Get(hNotAfter),
		SigPresent:  r.Header.Get(hSignature) != "",
		KeyPresent:  key != "",
		SigValid:    ok,
		SigReason:   reason,
	}
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	rep := build(r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(rep)
	log.Printf("http %s host=%s verify=%q sig_valid=%v", r.Method, r.Host, rep.Verify, rep.SigValid)
}

// writeText sends one unmasked server-to-client text frame.
func writeText(w io.Writer, payload []byte) error {
	header := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xffff:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		header = append(append(header, 127), ext...)
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// drain reads client frames until the peer closes or the connection dies, so
// the sender notices a close promptly.
func drain(br *bufio.Reader, done chan<- struct{}) {
	defer close(done)
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(br, h); err != nil {
			return
		}
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(br, ext); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(ext))
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(br, ext); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(ext)
		}
		if masked {
			n += 4
		}
		if n > 1<<20 {
			return
		}
		if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
			return
		}
		if opcode == 0x8 {
			return
		}
	}
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		handleHTTP(w, r)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	rep := build(r)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := brw.Flush(); err != nil {
		return
	}
	log.Printf("ws open host=%s verify=%q sig_valid=%v", r.Host, rep.Verify, rep.SigValid)
	hello := fmt.Sprintf("hello verify=%s sig_valid=%v\n", rep.Verify, rep.SigValid)
	if err := writeText(conn, []byte(hello)); err != nil {
		return
	}
	done := make(chan struct{})
	go drain(brw.Reader, done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for i := 1; ; i++ {
		select {
		case <-done:
			log.Printf("ws closed by peer host=%s after %d ticks", r.Host, i-1)
			return
		case <-ticker.C:
			if err := writeText(conn, []byte(fmt.Sprintf("tick %d\n", i))); err != nil {
				log.Printf("ws write failed host=%s after %d ticks", r.Host, i-1)
				return
			}
		}
	}
}

// apiSummary is what `origin api` prints: enough to judge a call, never the
// certificate or chain an issue returns.
type apiSummary struct {
	Result       bool     `json:"result"`
	Message      string   `json:"message"`
	Code         string   `json:"code,omitempty"`
	Fingerprint  string   `json:"fingerprint,omitempty"`
	Hosts        []string `json:"hosts,omitempty"`
	Fingerprints []string `json:"fingerprints,omitempty"`
}

// callAPI is `origin api <action>`: the app side of the agent's local API, as
// an app with a grant sees it (ODAC_API_SOCKET, ODAC_API_KEY). The data array
// arrives on stdin so a CSR never sits in argv. It always prints one summary
// and exits 0, a transport failure included, so a caller can judge it.
func callAPI(action string) int {
	fail := func(code, message string) int {
		_ = json.NewEncoder(os.Stdout).Encode(apiSummary{Code: code, Message: message})
		return 0
	}
	socket, key := os.Getenv("ODAC_API_SOCKET"), os.Getenv("ODAC_API_KEY")
	if socket == "" || key == "" {
		return fail("no_grant", "no API grant in this container")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return fail("transport", "read stdin: "+err.Error())
	}
	data := json.RawMessage("[]")
	if len(strings.TrimSpace(string(raw))) > 0 {
		data = raw
	}
	req, err := json.Marshal(map[string]any{"auth": key, "action": action, "data": data})
	if err != nil {
		return fail("transport", "encode: "+err.Error())
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return fail("transport", "dial: "+err.Error())
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write(req); err != nil {
		return fail("transport", "write: "+err.Error())
	}
	// Progress frames first, then the final one, which carries "result". A
	// refusal leaves the connection open, so read frames, not to EOF.
	var resp struct {
		Result  *bool           `json:"result"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	dec := json.NewDecoder(io.LimitReader(conn, 4<<20))
	for resp.Result == nil {
		resp.Data = nil
		if err := dec.Decode(&resp); err != nil {
			return fail("transport", "read: "+err.Error())
		}
	}
	out := apiSummary{Result: *resp.Result, Message: resp.Message}
	if len(out.Message) > 200 {
		out.Message = out.Message[:200]
	}
	var obj struct {
		Code        string   `json:"code"`
		Fingerprint string   `json:"fingerprint"`
		Hosts       []string `json:"hosts"`
	}
	var rows []struct {
		Fingerprint string `json:"fingerprint"`
	}
	if json.Unmarshal(resp.Data, &obj) == nil {
		out.Code, out.Fingerprint, out.Hosts = obj.Code, obj.Fingerprint, obj.Hosts
	} else if json.Unmarshal(resp.Data, &rows) == nil {
		out.Fingerprints = []string{}
		for _, row := range rows {
			out.Fingerprints = append(out.Fingerprints, row.Fingerprint)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	return 0
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "api" {
		os.Exit(callAPI(os.Args[2]))
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleWS)
	mux.HandleFunc("/", handleHTTP)
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Printf("mtls-e2e origin listening on %s (key_present=%v)", addr, os.Getenv("ODAC_CLIENT_CERT_KEY") != "")
	log.Fatal(http.ListenAndServe(addr, mux))
}
