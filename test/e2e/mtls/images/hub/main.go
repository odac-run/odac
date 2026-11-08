// hub is the test stand-in for ODAC Cloud. Nothing in a run reaches the real one.
//
// Two faces:
//
//   - :8080, plain HTTP, the agent's side: POST /app serves the recipes behind
//     `odac app create <recipe>` (internal/hub/http.go GetApp in the agent).
//     The harness drives the edge protocol through /ctl/* on the same port.
//   - :8443, TLS with the run's own CA, the edge side: odac-tunnel dials
//     wss://hub.mtls.test:8443/tunnel and the hub speaks MTLS_PLAN.md §6 to it. The real
//     dashboard handler does not exist yet; this is a faithful stand-in for it,
//     including the per-edge queue that holds requests for an offline node.
//
// Request and reply bodies are never logged: they carry CSRs, certificates and
// tokens, and the harness greps every log for exactly those.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var edgeIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

const replyTimeout = 25 * time.Second

type tunnelRecord struct {
	Domain string `json:"domain"`
	Token  string `json:"token"`
	User   string `json:"user"`
}

type edge struct {
	id   string
	conn *websocket.Conn
	wmu  sync.Mutex
}

func (e *edge) send(v any) error {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_ = e.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return e.conn.WriteJSON(v)
}

type queued struct {
	id  string
	msg map[string]any
}

type hub struct {
	secret string
	token  string

	mu      sync.Mutex
	seq     int
	edges   map[string]*edge
	blocked map[string]bool
	queue   map[string][]queued
	flushed map[string][]json.RawMessage
	pending map[string]chan json.RawMessage
	tunnels map[string]tunnelRecord
}

func newHub() *hub {
	return &hub{
		secret:  os.Getenv("E2E_TUNNEL_SECRET"),
		token:   os.Getenv("E2E_TUNNEL_TOKEN"),
		edges:   map[string]*edge{},
		blocked: map[string]bool{},
		queue:   map[string][]queued{},
		flushed: map[string][]json.RawMessage{},
		pending: map[string]chan json.RawMessage{},
		tunnels: map[string]tunnelRecord{},
	}
}

// --- recipes ---

func (h *hub) recipes(name string) (map[string]any, bool) {
	app := func(appName string) map[string]any {
		return map[string]any{
			"name":  appName,
			"image": "mtls-e2e/origin:local",
			"ports": []any{map[string]any{"container": 8080}},
		}
	}
	tunnelApp := func(edgeID string) map[string]any {
		return map[string]any{
			"name":  "odac-tunnel",
			"image": "mtls-e2e/odac-tunnel:local",
			"ports": []any{map[string]any{"container": 8080}},
			"env": map[string]any{
				"ODAC_TUNNEL_SECRET": h.secret,
				"ODAC_EDGE_ID":       edgeID,
				"ODAC_HUB_URL":       "wss://hub.mtls.test:8443/tunnel",
				"SSL_CERT_FILE":      "/e2e/pki/root.pem",
			},
			"volumes": []any{map[string]any{"host": "/e2e/pki", "container": "/e2e/pki:ro"}},
		}
	}
	switch name {
	case "mtls-origin":
		return app("origin"), true
	case "mtls-other":
		return app("other"), true
	case "mtls-home":
		return app("home"), true
	case "odac-tunnel-a":
		return tunnelApp("edge-a"), true
	case "odac-tunnel-b":
		return tunnelApp("edge-b"), true
	}
	return nil, false
}

func reply(w http.ResponseWriter, status int, success bool, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": map[string]any{"success": success, "message": message},
		"data":   data,
	})
}

func (h *hub) handleApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		reply(w, http.StatusMethodNotAllowed, false, "e2e hub: POST only", nil)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, false, "e2e hub: bad body", nil)
		return
	}
	recipe, ok := h.recipes(body.Name)
	if !ok {
		log.Printf("action=app recipe=%q result=unknown", body.Name)
		reply(w, http.StatusOK, false, "e2e hub: unknown recipe", nil)
		return
	}
	log.Printf("action=app recipe=%q result=ok", body.Name)
	reply(w, http.StatusOK, true, "", recipe)
}

// --- edge side (§6) ---

var upgrader = websocket.Upgrader{ReadBufferSize: 16 << 10, WriteBufferSize: 16 << 10}

func (h *hub) handleTunnel(w http.ResponseWriter, r *http.Request) {
	given := r.Header.Get("x-odac-internal-secret")
	if h.secret == "" || subtle.ConstantTimeCompare([]byte(given), []byte(h.secret)) != 1 {
		log.Printf("edge refused: bad secret")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.Header.Get("x-odac-edge-id")
	if !edgeIDPattern.MatchString(id) {
		log.Printf("edge refused: missing or malformed edge id")
		http.Error(w, "edge id required", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	blocked := h.blocked[id]
	h.mu.Unlock()
	if blocked {
		log.Printf("edge=%s refused: blocked by the harness", logSafe(id))
		http.Error(w, "edge blocked", http.StatusServiceUnavailable)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	e := &edge{id: id, conn: conn}
	h.mu.Lock()
	if old := h.edges[id]; old != nil {
		old.conn.Close()
	}
	h.edges[id] = e
	tunnels := make([]tunnelRecord, 0, len(h.tunnels))
	for _, t := range h.tunnels {
		tunnels = append(tunnels, t)
	}
	h.mu.Unlock()
	log.Printf("edge=%s connected", logSafe(id))

	_ = e.send(map[string]any{"action": "sync", "tunnels": tunnels})
	go h.flush(id)

	defer func() {
		h.mu.Lock()
		if h.edges[id] == e {
			delete(h.edges, id)
		}
		h.mu.Unlock()
		conn.Close()
		log.Printf("edge=%s disconnected", logSafe(id))
	}()
	conn.SetReadLimit(1 << 20)
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var head struct {
			Action string `json:"action"`
			ID     string `json:"id"`
			Edge   string `json:"edge"`
			OK     bool   `json:"ok"`
			Code   string `json:"code"`
		}
		if json.Unmarshal(raw, &head) != nil {
			log.Printf("edge=%s sent an unreadable message size=%d", logSafe(id), len(raw))
			continue
		}
		switch head.Action {
		case "cert.result", "cert.revoked", "cert.imported":
			log.Printf("edge=%s reply action=%s id=%s ok=%v code=%q reply_edge=%s", logSafe(id), logSafe(head.Action), logSafe(head.ID), head.OK, logSafe(head.Code), logSafe(head.Edge))
			h.mu.Lock()
			ch := h.pending[head.ID]
			delete(h.pending, head.ID)
			h.mu.Unlock()
			if ch != nil {
				ch <- json.RawMessage(raw)
			}
		case "bandwidth":
		default:
			log.Printf("edge=%s unknown action=%q", logSafe(id), logSafe(head.Action))
		}
	}
}

// exchange sends one request to a connected edge and waits for its reply.
func (h *hub) exchange(e *edge, id string, msg map[string]any) (json.RawMessage, error) {
	ch := make(chan json.RawMessage, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	msg["id"] = id
	log.Printf("edge=%s request action=%s id=%s", logSafe(e.id), logSafe(msg["action"]), logSafe(id))
	if err := e.send(msg); err != nil {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, err
	}
	select {
	case raw := <-ch:
		return raw, nil
	case <-time.After(replyTimeout):
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, fmt.Errorf("timeout")
	}
}

// flush sends what was queued for an edge while it was away, in order (T8).
// A request whose reply does not come stays at the head of the queue.
func (h *hub) flush(id string) {
	for {
		h.mu.Lock()
		e := h.edges[id]
		if e == nil || len(h.queue[id]) == 0 {
			h.mu.Unlock()
			return
		}
		next := h.queue[id][0]
		h.mu.Unlock()
		raw, err := h.exchange(e, next.id, next.msg)
		if err != nil {
			log.Printf("edge=%s flush stopped at id=%s: %s", logSafe(id), logSafe(next.id), logSafe(err))
			return
		}
		h.mu.Lock()
		h.queue[id] = h.queue[id][1:]
		h.flushed[id] = append(h.flushed[id], raw)
		h.mu.Unlock()
		log.Printf("edge=%s flushed id=%s", logSafe(id), logSafe(next.id))
	}
}

func (h *hub) broadcast(msg map[string]any) {
	h.mu.Lock()
	edges := make([]*edge, 0, len(h.edges))
	for _, e := range h.edges {
		edges = append(edges, e)
	}
	h.mu.Unlock()
	for _, e := range edges {
		if err := e.send(msg); err != nil {
			log.Printf("edge=%s broadcast failed: %s", logSafe(e.id), logSafe(err))
		}
	}
}

// --- harness control (/ctl) ---

// logSafe renders a value another party sent for a log line, without the line
// breaks that would let it forge further lines: the scenarios read these logs.
func logSafe(v any) string {
	s := fmt.Sprint(v)
	s = strings.ReplaceAll(s, "\n", "")
	return strings.ReplaceAll(s, "\r", "")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *hub) handleCtl(w http.ResponseWriter, r *http.Request) {
	edgeID := r.URL.Query().Get("edge")
	switch strings.TrimPrefix(r.URL.Path, "/ctl/") {
	case "edges":
		h.mu.Lock()
		ids := []string{}
		for id := range h.edges {
			ids = append(ids, id)
		}
		queuedN := map[string]int{}
		for id, q := range h.queue {
			queuedN[id] = len(q)
		}
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"edges": ids, "queued": queuedN})

	case "tunnel":
		// {domain, user}: the token is the run's tunnel token, never sent here.
		var t tunnelRecord
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&t); err != nil || t.Domain == "" || t.User == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "domain and user required"})
			return
		}
		t.Token = h.token
		h.mu.Lock()
		h.tunnels[t.Domain] = t
		h.mu.Unlock()
		log.Printf("tunnel upsert domain=%s user=%s", logSafe(t.Domain), logSafe(t.User))
		h.broadcast(map[string]any{"action": "upsert", "tokenType": "agent", "domain": t.Domain, "token": t.Token, "user": t.User})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "request":
		var msg map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&msg); err != nil || !edgeIDPattern.MatchString(edgeID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
			return
		}
		h.mu.Lock()
		h.seq++
		id := fmt.Sprintf("e2e-%d", h.seq)
		e := h.edges[edgeID]
		if e == nil {
			if r.URL.Query().Get("queue") == "1" {
				h.queue[edgeID] = append(h.queue[edgeID], queued{id: id, msg: msg})
				h.mu.Unlock()
				log.Printf("edge=%s offline, queued action=%s id=%s", logSafe(edgeID), logSafe(msg["action"]), logSafe(id))
				writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "id": id})
				return
			}
			h.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "edge offline"})
			return
		}
		h.mu.Unlock()
		raw, err := h.exchange(e, id, msg)
		if err != nil {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)

	case "flushed":
		h.mu.Lock()
		out := h.flushed[edgeID]
		h.mu.Unlock()
		if out == nil {
			out = []json.RawMessage{}
		}
		writeJSON(w, http.StatusOK, out)

	case "block":
		if !edgeIDPattern.MatchString(edgeID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
			return
		}
		on := r.URL.Query().Get("on") == "1"
		h.mu.Lock()
		h.blocked[edgeID] = on
		e := h.edges[edgeID]
		h.mu.Unlock()
		if on && e != nil {
			e.conn.Close()
		}
		log.Printf("edge=%s blocked=%v", logSafe(edgeID), on)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown"})
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	h := newHub()

	plain := http.NewServeMux()
	plain.HandleFunc("/app", h.handleApp)
	plain.HandleFunc("/ctl/", h.handleCtl)
	go func() {
		log.Printf("mtls-e2e hub: agent side on :8080")
		log.Fatal(http.ListenAndServe(":8080", plain))
	}()

	cert, key := "/e2e/pki/server.pem", "/e2e/pki/server.key"
	if _, err := os.Stat(cert); err != nil {
		log.Printf("mtls-e2e hub: no %s, edge side disabled", cert)
		select {}
	}
	edges := http.NewServeMux()
	edges.HandleFunc("/tunnel", h.handleTunnel)
	log.Printf("mtls-e2e hub: edge side on :8443")
	log.Fatal(http.ListenAndServeTLS(":8443", cert, key, edges))
}
