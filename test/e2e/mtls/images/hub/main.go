// hub is the test stand-in for hub.odac.run. Nodes point ODAC_HUB_URL at it, so
// nothing in the run reaches the real Cloud.
//
// Today it answers the one HTTP call the custom-domain scenarios need: the
// recipe fetch behind `odac app create <recipe>` (POST /app, see the agent's
// internal/hub/http.go GetApp). Every other action is refused with the same
// envelope shape the agent parses, so an unexpected call shows up as a failure
// rather than as a silent success.
//
// The edge/tunnel scenarios will grow a /tunnel WebSocket here that speaks
// MTLS_PLAN.md §6 to odac-tunnel. Request bodies are never logged: they carry
// CSRs and tokens, and the harness greps every log for exactly those.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
)

// recipes are served by name. The image is loaded into each node's engine by
// run.sh before any app is created, so the agent never pulls.
var recipes = map[string]map[string]any{
	"mtls-origin": {
		"name":  "origin",
		"image": "mtls-e2e/origin:local",
		"ports": []any{map[string]any{"container": 8080}},
	},
}

func reply(w http.ResponseWriter, status int, success bool, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": map[string]any{"success": success, "message": message},
		"data":   data,
	})
}

func handle(w http.ResponseWriter, r *http.Request) {
	action := strings.Trim(r.URL.Path, "/")
	if r.Method != http.MethodPost {
		reply(w, http.StatusMethodNotAllowed, false, "e2e hub: POST only", nil)
		return
	}
	switch action {
	case "app":
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			reply(w, http.StatusBadRequest, false, "e2e hub: bad body", nil)
			return
		}
		recipe, ok := recipes[body.Name]
		if !ok {
			log.Printf("action=app recipe=%q result=unknown", body.Name)
			reply(w, http.StatusOK, false, "e2e hub: unknown recipe", nil)
			return
		}
		log.Printf("action=app recipe=%q result=ok", body.Name)
		reply(w, http.StatusOK, true, "", recipe)
	default:
		log.Printf("action=%q result=refused", action)
		reply(w, http.StatusOK, false, "e2e hub: action not implemented", nil)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	http.HandleFunc("/", handle)
	log.Printf("mtls-e2e hub listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
