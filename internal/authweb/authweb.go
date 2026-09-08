// Package authweb is the login web app behind the button-auth flow: the page a
// MetaMCP button opens, plus the endpoints that drive an interactive store login
// and persist the captured token into the same encrypted store the MCP tools
// read. See docs/button-auth.md.
package authweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/browserlogin"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
)

// Backend is the slice of the MCP server the login app needs: it validates and
// stores credentials exactly as the receipts_login tool does, and reports state.
type Backend interface {
	WebLogin(ctx context.Context, providerID string, fields map[string]string) (provider.LoginResult, error)
	StatusReport(ctx context.Context) (any, error)
}

// Handler serves the login app.
type Handler struct {
	backend  Backend
	sessions *browserlogin.Manager
	chromium string
	prefix   string
}

// New builds the login handler. chromiumPath enables the browser-driven path;
// empty falls back to letting chromedp find a browser on PATH.
func New(backend Backend, chromiumPath string) *Handler {
	return &Handler{
		backend:  backend,
		sessions: browserlogin.NewManager(),
		chromium: chromiumPath,
	}
}

// Mount registers the login routes on mux under the given prefix (e.g. "/login").
// It also registers "/status" and "/healthz" at the root.
func (h *Handler) Mount(mux *http.ServeMux, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		prefix = "/login"
	}
	mux.HandleFunc(prefix, h.page)
	mux.HandleFunc(prefix+"/", h.page)
	mux.HandleFunc(prefix+"/paste", h.paste)
	mux.HandleFunc(prefix+"/start", h.start)
	mux.HandleFunc(prefix+"/step", h.step)
	mux.HandleFunc(prefix+"/cancel", h.cancel)
	mux.HandleFunc("/status", h.status)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h.prefix = prefix
}

// guidedDriver returns the browser-driven driver for a provider, when one
// exists. Only Lidl has a reliable automated login today.
func (h *Handler) guidedDriver(providerID string) (browserlogin.Driver, bool) {
	switch providerID {
	case "lidl":
		return browserlogin.NewLidlDriver(h.chromium), true
	default:
		return nil, false
	}
}

// --- interactive (browser-driven) endpoints ---

type startRequest struct {
	Provider string `json:"provider"`
}

type stepRequest struct {
	Session string            `json:"session"`
	Values  map[string]string `json:"values"`
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	driver, ok := h.guidedDriver(strings.ToLower(strings.TrimSpace(req.Provider)))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("no guided login for %q; use the paste form", req.Provider))
		return
	}
	result, err := h.sessions.Start(r.Context(), driver)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	h.finishIfDone(w, r, result)
}

func (h *Handler) step(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req stepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	result, err := h.sessions.Step(r.Context(), req.Session, req.Values)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	h.finishIfDone(w, r, result)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var req stepRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Session != "" {
		h.sessions.Cancel(req.Session)
	}
	writeJSON(w, map[string]any{"ok": true})
}

// finishIfDone persists a completed login's captured fields via the backend,
// then returns the step result (with the backend's message on success).
func (h *Handler) finishIfDone(w http.ResponseWriter, r *http.Request, result browserlogin.StepResult) {
	if !result.Done {
		writeJSON(w, result)
		return
	}
	login, err := h.backend.WebLogin(r.Context(), result.Provider, result.Stored)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]any{
		"done":     true,
		"provider": result.Provider,
		"ok":       login.OK,
		"message":  firstNonEmpty(login.Message, result.Message),
	})
}

// --- paste (no-JS) endpoint ---

func (h *Handler) paste(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	providerID := strings.ToLower(strings.TrimSpace(r.FormValue("provider")))
	fields := map[string]string{}
	for key, vals := range r.PostForm {
		if key == "provider" || len(vals) == 0 {
			continue
		}
		if v := strings.TrimSpace(vals[0]); v != "" {
			fields[key] = v
		}
	}
	login, err := h.backend.WebLogin(r.Context(), providerID, fields)
	if err != nil {
		h.renderPage(w, r, fmt.Sprintf("%s: %v", providerID, err))
		return
	}
	msg := login.Message
	if msg == "" {
		msg = providerID + ": saved"
	}
	http.Redirect(w, r, h.prefix+"?msg="+urlEscape(msg), http.StatusSeeOther)
}

// --- status ---

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	report, err := h.backend.StatusReport(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, report)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
