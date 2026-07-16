// http.go ports Hub.js's HTTP side: call() (30s timeout, TLS verification,
// 3 attempts on DNS/timeout failures with linear backoff), auth() and
// getApp(), plus the envelope quirks pinned in contract 0.2:
// `result.authenticated === false` returns the result as-is instead of
// throwing, and the auth() catch block feeds the THROWN VALUE into
// Api.result — an Error instance JSON-stringifies to {}, so those failures
// answer {"result":false,"message":null,"data":{}} exactly like Node.
package hub

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"odac/internal/api"
	"odac/internal/jscanon"
	"odac/internal/lang"
)

const httpTimeout = 30 * time.Second

// httpResponse mirrors core/Http.js's {status, data} (data JSON-parsed when
// the content type says so, raw text otherwise).
type httpResponse struct {
	status int
	data   any
}

// defaultPostJSON is the production transport (test seam on Hub.postJSON).
func defaultPostJSON(url string, body []byte, headers map[string]string) (*httpResponse, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: httpTimeout} // TLS verification enforced (default)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	out := &httpResponse{status: resp.StatusCode}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		if len(raw) == 0 {
			out.data = nil
		} else if json.Unmarshal(raw, &out.data) != nil {
			out.data = string(raw)
		}
	} else {
		out.data = string(raw)
	}
	return out, nil
}

// thrown models a JS `throw`: either an Error instance (message, zero
// enumerable props) or an arbitrary thrown value (error.response.data).
type thrown struct {
	isErrorObject bool
	message       string
	value         any
}

func (t *thrown) Error() string {
	if t.isErrorObject {
		return t.message
	}
	if s, ok := t.value.(string); ok {
		return s
	}
	// Node call sites read err.message off the thrown value; a plain object
	// has none, and util-style formatting renders that as "undefined".
	return "undefined"
}

func throwError(format string, args ...any) *thrown {
	return &thrown{isErrorObject: true, message: fmt.Sprintf(format, args...)}
}

// call ports Hub.call(action, data): returns the envelope's `data` on
// success (or the raw result object on the authenticated:false quirk).
func (h *Hub) call(action string, data any) (any, *thrown) {
	h.log.Log("Hub API call: %s", action)

	url := h.baseURL + "/" + action
	body, err := jscanon.Marshal(data)
	if err != nil {
		return nil, throwError("%s", err.Error())
	}
	headers := h.buildHeaders(action, data, body)

	const retries = 3
	for attempt := 1; attempt <= retries; attempt++ {
		resp, err := h.postJSON(url, body, headers)
		if err != nil {
			code := errCode(err, url)
			retryable := code == "EAI_AGAIN" || code == "ENOTFOUND" || code == "ETIMEDOUT"
			if retryable && attempt < retries {
				h.log.Log("Hub API call failed (attempt %s/%s): %s - Retrying...",
					fmt.Sprint(attempt), fmt.Sprint(retries), code)
				h.sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return nil, h.handleAPIError(action, err, url)
		}
		if resp.status < 200 || resp.status >= 300 {
			// core/Http rejects with error.response; #handleApiError then
			// throws error.response.data (the parsed body, whatever it is).
			h.log.Log("API call failed: %s - %s", action, fmt.Sprintf("HTTP Error %d", resp.status))
			if resp.data == nil {
				// throw of null: `error || …` fallbacks treat it as absent.
				return nil, &thrown{}
			}
			return nil, &thrown{value: resp.data}
		}
		return h.parseResponse(action, resp)
	}
	return nil, throwError("unreachable") // loop always returns
}

// parseResponse ports #parseResponse.
func (h *Hub) parseResponse(action string, resp *httpResponse) (any, *thrown) {
	envelope, _ := resp.data.(map[string]any)
	result, _ := envelope["result"].(map[string]any)
	if !jsTruthy(envelope["result"]) {
		return nil, throwError("Invalid response format")
	}
	if !jsTruthy(result["success"]) {
		if result["authenticated"] == false {
			return result, nil
		}
		return nil, throwError("%s", str(result["message"]))
	}
	h.log.Log("API call successful: %s", action)
	return envelope["data"], nil
}

// handleAPIError ports #handleApiError's non-response branches (the
// response branch is handled inline in call). Node's core/Http rewrites
// ECONNREFUSED and timeout messages; those are load-bearing for parity
// (the CLI shows them), other transport errors keep Go's text.
func (h *Hub) handleAPIError(action string, err error, url string) *thrown {
	message := err.Error()
	if errors.Is(err, syscall.ECONNREFUSED) {
		message = "Connection refused at " + url
	} else if isTimeout(err) {
		message = fmt.Sprintf("Request timed out after %dms", httpTimeout.Milliseconds())
	}
	h.log.Log("API call failed: %s - %s", action, message)
	return &thrown{isErrorObject: true, message: message}
}

// errCode maps a Go transport error to the Node error codes call() retries
// on.
func errCode(err error, _ string) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTemporary {
			return "EAI_AGAIN"
		}
		return "ENOTFOUND"
	}
	if isTimeout(err) {
		return "ETIMEDOUT"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "ECONNREFUSED"
	}
	return ""
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// buildHeaders ports #buildHeaders: bearer token when authenticated, and
// X-Signature (HMAC over the exact body bytes) for non-auth calls whose
// payload carries a timestamp.
func (h *Hub) buildHeaders(action string, data any, body []byte) map[string]string {
	headers := map[string]string{}
	token, secret := h.hubConfig()
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	if action != "auth" && jsTruthy(fieldOf(data, "timestamp")) && secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		headers["X-Signature"] = hex.EncodeToString(mac.Sum(nil))
	}
	return headers
}

// fieldOf reads a property off an object-ish payload (map or jscanon.Obj).
func fieldOf(data any, key string) any {
	switch d := data.(type) {
	case map[string]any:
		return d[key]
	case jscanon.Obj:
		for _, f := range d {
			if f.K == key {
				return f.V
			}
		}
	}
	return nil
}

// Auth ports Hub.auth(code): the one-time-code exchange for {token,
// secret}. Registered as the `auth` api action.
func (h *Hub) Auth(code any) api.Result {
	h.log.Log("Odac authenticating...")
	display := "none"
	if jsTruthy(code) {
		prefix := str(code)
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		display = prefix + "..."
	}
	h.log.Log("Auth code received: %s", display)

	data := jscanon.Obj{{K: "code", V: code}}
	if h.deps.SysInfo != nil {
		data = append(data, h.deps.SysInfo()...) // {code, ...System.info()}
	}

	response, terr := h.call("auth", data)
	if terr == nil {
		resp, _ := response.(map[string]any)
		token, tokOK := resp["token"].(string)
		secret, secOK := resp["secret"].(string)
		if !tokOK || !secOK {
			// Node throws an Error caught by the same catch block below.
			terr = throwError("%s", lang.T("Invalid authentication response format"))
		} else {
			h.cfg.Set("hub", map[string]any{"token": token, "secret": secret})
			h.log.Log("Odac authenticated!")
			return api.Res(true, lang.T("Authentication successful"))
		}
	}

	h.log.Log("Authentication failed: %s", terr.Error())
	// Node: result(false, error || 'Authentication failed') with the THROWN
	// VALUE — an Error instance stringifies to {} (data slot, null message).
	if terr.isErrorObject {
		return api.Res(false, map[string]any{})
	}
	if terr.value == nil {
		return api.Res(false, lang.T("Authentication failed"))
	}
	return api.Res(false, terr.value)
}

// GetApp ports Hub.getApp(name): the recipe fetch behind app.create — the
// appmgr Hub seam.
func (h *Hub) GetApp(name string) (map[string]any, error) {
	data, terr := h.call("app", map[string]any{"name": name})
	if terr != nil {
		return nil, terr
	}
	recipe, _ := data.(map[string]any)
	if recipe == nil {
		recipe = map[string]any{}
	}
	return recipe, nil
}
