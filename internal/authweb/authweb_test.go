package authweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
)

// fakeBackend records what WebLogin was asked to store and reports a fixed status.
type fakeBackend struct {
	lastProvider string
	lastFields   map[string]string
	fail         bool
}

func (b *fakeBackend) WebLogin(_ context.Context, providerID string, fields map[string]string) (provider.LoginResult, error) {
	b.lastProvider = providerID
	b.lastFields = fields
	if b.fail {
		return provider.LoginResult{}, http.ErrAbortHandler
	}
	return provider.LoginResult{Provider: providerID, OK: true, Message: providerID + " ok"}, nil
}

func (b *fakeBackend) StatusReport(context.Context) (any, error) {
	return map[string]any{
		"providers": []map[string]any{
			{"provider": "lidl", "logged_in": true},
			{"provider": "action", "logged_in": false},
			{"provider": "allegro", "logged_in": false},
		},
		"mailbox": map[string]any{"configured": false},
	}, nil
}

func newTestHandler(b Backend) (*Handler, *http.ServeMux) {
	h := New(b, "")
	mux := http.NewServeMux()
	h.Mount(mux, "/login")
	return h, mux
}

func TestPageRenders(t *testing.T) {
	_, mux := newTestHandler(&fakeBackend{})
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Lidl Plus", "Action", "Allegro", "Guided login", "connected"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestPastePersistsAndRedirects(t *testing.T) {
	backend := &fakeBackend{}
	_, mux := newTestHandler(backend)

	form := url.Values{"provider": {"allegro"}, "cookie": {"QXLSESSID=abc"}}
	req := httptest.NewRequest(http.MethodPost, "/login/paste", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	if backend.lastProvider != "allegro" || backend.lastFields["cookie"] != "QXLSESSID=abc" {
		t.Fatalf("backend got provider=%q fields=%+v", backend.lastProvider, backend.lastFields)
	}
	if p, ok := backend.lastFields["provider"]; ok {
		t.Errorf("the provider selector leaked into stored fields: %q", p)
	}
}

func TestStatusIsJSON(t *testing.T) {
	_, mux := newTestHandler(&fakeBackend{})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("status not JSON: %v", err)
	}
	if _, ok := parsed["providers"]; !ok {
		t.Error("status missing providers")
	}
}

func TestGuidedStartRejectsUnknownProvider(t *testing.T) {
	_, mux := newTestHandler(&fakeBackend{})
	body, _ := json.Marshal(startRequest{Provider: "allegro"}) // no guided driver
	req := httptest.NewRequest(http.MethodPost, "/login/start", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a provider without guided login", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	_, mux := newTestHandler(&fakeBackend{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}
