package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"odac/internal/mail/auth"
	"odac/internal/mail/config"
	"odac/internal/mail/storage"
)

func newTestServer(t *testing.T) (*Server, *storage.Store) {
	t.Helper()
	store, err := storage.NewStore(filepath.Join(t.TempDir(), "mail"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewServer(store, auth.NewFirewall(), nil), store
}

func call(t *testing.T, s *Server, method, path, body string) (int, apiResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp apiResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestAccountLifecycle(t *testing.T) {
	s, store := newTestServer(t)
	ctx := context.Background()

	code, resp := call(t, s, http.MethodPost, "/account",
		`{"domain":"Example.com","email":"Ali@Example.com","password":"pw1","retype":"pw1"}`)
	if code != http.StatusOK || !resp.Success {
		t.Fatalf("create: %d %+v", code, resp)
	}
	row, err := store.AccountExists(ctx, "ali@example.com")
	if err != nil || row == nil {
		t.Fatalf("account not stored: %v", err)
	}
	if row.Email != "ali@example.com" || row.Domain != "example.com" {
		t.Fatalf("stored %q / %q, want the canonical spelling", row.Email, row.Domain)
	}
	if row.Password == "pw1" {
		t.Fatal("password stored in clear")
	}
	if ok, _ := auth.ComparePassword("pw1", row.Password); !ok {
		t.Fatal("stored hash does not match the password")
	}

	code, resp = call(t, s, http.MethodPost, "/account",
		`{"email":"ALI@example.com","password":"x","retype":"x"}`)
	if code != http.StatusConflict {
		t.Fatalf("duplicate in another case: %d %+v", code, resp)
	}

	code, _ = call(t, s, http.MethodPut, "/account/password",
		`{"email":"ali@example.com","password":"pw2","retype":"pw2"}`)
	if code != http.StatusOK {
		t.Fatalf("password update: %d", code)
	}
	row, _ = store.AccountExists(ctx, "ali@example.com")
	if ok, _ := auth.ComparePassword("pw2", row.Password); !ok {
		t.Fatal("password not updated")
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts?domain=EXAMPLE.com", nil))
	var list struct {
		Accounts []storage.AccountEntry `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Accounts) != 1 {
		t.Fatalf("list: %s", rec.Body.String())
	}

	code, _ = call(t, s, http.MethodDelete, "/account", `{"email":"ALI@example.com"}`)
	if code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if row, _ := store.AccountExists(ctx, "ali@example.com"); row != nil {
		t.Fatal("account still present after delete")
	}
	code, _ = call(t, s, http.MethodDelete, "/account", `{"email":"ali@example.com"}`)
	if code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", code)
	}
}

func TestAccountRequestsAreValidated(t *testing.T) {
	s, store := newTestServer(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/account", `not json`, http.StatusBadRequest},
		{http.MethodPost, "/account", `{"email":"a@b.co","password":"x"}`, http.StatusBadRequest},
		{http.MethodPost, "/account", `{"email":"a@b.co","password":"x","retype":"y"}`, http.StatusBadRequest},
		{http.MethodPost, "/account", `{"email":"not-an-address","password":"x","retype":"x"}`, http.StatusBadRequest},
		{http.MethodPost, "/account", `{"email":"a@b.co\r\nBcc: x@y.z","password":"x","retype":"x"}`, http.StatusBadRequest},
		{http.MethodPut, "/account/password", `{"email":"nobody@b.co","password":"x","retype":"x"}`, http.StatusNotFound},
		{http.MethodPut, "/account/password", `{"email":"a@b.co","password":"x","retype":"y"}`, http.StatusBadRequest},
		{http.MethodDelete, "/account", `{}`, http.StatusBadRequest},
		{http.MethodGet, "/account", ``, http.StatusMethodNotAllowed},
		{http.MethodGet, "/account/password", ``, http.StatusMethodNotAllowed},
		{http.MethodPost, "/accounts", ``, http.StatusMethodNotAllowed},
		{http.MethodGet, "/nowhere", ``, http.StatusNotFound},
	} {
		if code, _ := call(t, s, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s %q: %d, want %d", tc.method, tc.path, tc.body, code, tc.want)
		}
	}
	all, err := store.AccountListAll(context.Background())
	if err != nil || len(all) != 0 {
		t.Fatalf("refused requests stored accounts: %v %v", all, err)
	}
}

func TestSendQueuesThroughTheCallback(t *testing.T) {
	s, _ := newTestServer(t)

	if code, _ := call(t, s, http.MethodPost, "/send", `{"from":"a@b.co","to":"c@d.co","body":"x"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("send with no queue: %d, want 503", code)
	}

	var got []string
	s.SetSendCallback(func(from, to string, body []byte) error {
		got = append(got, from, to, string(body))
		if to == "fail@d.co" {
			return errors.New("disk full")
		}
		return nil
	})
	if code, resp := call(t, s, http.MethodPost, "/send", `{"from":"a@b.co","to":"c@d.co","body":"hello"}`); code != http.StatusOK || !resp.Success {
		t.Fatalf("send: %d %+v", code, resp)
	}
	if strings.Join(got, "|") != "a@b.co|c@d.co|hello" {
		t.Fatalf("callback got %v", got)
	}
	if code, _ := call(t, s, http.MethodPost, "/send", `{"from":"a@b.co","to":"fail@d.co","body":"x"}`); code != http.StatusInternalServerError {
		t.Fatalf("failed queue: %d, want 500", code)
	}
	if code, _ := call(t, s, http.MethodPost, "/send", `{"from":"a@b.co","body":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("missing recipient: %d, want 400", code)
	}
}

func TestConfigAndSSLClearReachTheirCallbacks(t *testing.T) {
	var cfg config.Config
	var cleared []string
	s, _ := newTestServer(t)
	s.onConfig = func(c config.Config) { cfg = c }
	s.SetSSLClearCallback(func(d string) { cleared = append(cleared, d) })

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(`{"hostname":"mail.example.com"}`)))
	if rec.Code != http.StatusOK || cfg.Hostname != "mail.example.com" {
		t.Fatalf("config: %d %+v", rec.Code, cfg)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(`{`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad config: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ssl/clear", strings.NewReader(`{"domain":"example.com"}`)))
	if rec.Code != http.StatusOK || len(cleared) != 1 || cleared[0] != "example.com" {
		t.Fatalf("ssl clear: %d %v", rec.Code, cleared)
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
}
