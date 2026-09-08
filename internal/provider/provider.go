// Package provider defines what a receipt source has to be able to do, and
// wires several sources for the same store into one.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
)

// Sentinel errors every provider is expected to use, so callers can react
// without matching on message text.
var (
	// ErrNotLoggedIn means no usable credentials are stored yet.
	ErrNotLoggedIn = errors.New("not logged in")
	// ErrCredentialsRejected means the stored credentials no longer work.
	ErrCredentialsRejected = errors.New("credentials rejected, log in again")
	// ErrEndpointUnavailable means the store's private endpoint answered in a
	// way we cannot use — it moved, or it now demands a browser. This is the
	// signal that switches a provider to its fallback source.
	ErrEndpointUnavailable = errors.New("store endpoint unavailable")
	// ErrNotSupported means the provider cannot serve this call at all.
	ErrNotSupported = errors.New("not supported by this provider")
)

// Query selects which receipts to return.
type Query struct {
	From  time.Time
	To    time.Time
	Limit int
}

// Normalize fills in the defaults a provider can rely on.
func (q Query) Normalize() Query {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	return q
}

// LoginResult tells the caller what happened, without echoing any secret.
type LoginResult struct {
	Provider string   `json:"provider"`
	OK       bool     `json:"ok"`
	Message  string   `json:"message,omitempty"`
	Stored   []string `json:"stored_fields,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	// Next, when set, means the login is not finished: the caller must collect
	// these fields and call receipts_login again with the same Continuation. It
	// drives interactive logins (an SMS code, a confirmation) and maps onto the
	// MetaMCP Connect need_input step.
	Next *LoginNext `json:"next,omitempty"`
}

// LoginNext describes the next step of an interactive login.
type LoginNext struct {
	Prompt       string  `json:"prompt,omitempty"`
	Fields       []Field `json:"fields,omitempty"`
	Continuation string  `json:"continuation,omitempty"`
}

// Status describes a provider's readiness.
type Status struct {
	Provider     string   `json:"provider"`
	DisplayName  string   `json:"display_name"`
	LoggedIn     bool     `json:"logged_in"`
	StoredFields []string `json:"stored_fields,omitempty"`
	// Sources lists the data sources this provider will try, in order.
	Sources []string `json:"sources,omitempty"`
	// RequiredFields documents what receipts_login expects.
	RequiredFields []Field  `json:"required_fields,omitempty"`
	Notes          []string `json:"notes,omitempty"`
	// Error is the last problem detected, e.g. an expired token.
	Error string `json:"error,omitempty"`
}

// Field documents one credential a provider needs.
type Field struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
}

// Provider is one store's receipts.
type Provider interface {
	// ID is the stable short name used in tool arguments, e.g. "lidl".
	ID() string
	// DisplayName is the human name of the store.
	DisplayName() string
	// Status reports readiness without performing a network call.
	Status(ctx context.Context) Status
	// Login stores credentials and verifies them when it cheaply can.
	Login(ctx context.Context, fields map[string]string) (LoginResult, error)
	// Logout drops the stored credentials.
	Logout(ctx context.Context) error
	// List returns receipt summaries in the query window, newest first.
	List(ctx context.Context, q Query) ([]receipt.Receipt, error)
	// Get returns one receipt with its item lines.
	Get(ctx context.Context, id string) (receipt.Receipt, error)
}

// Registry holds the configured providers by id.
type Registry struct {
	byID map[string]Provider
}

// NewRegistry builds a registry from the given providers.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byID: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p != nil {
			r.byID[p.ID()] = p
		}
	}
	return r
}

// Get looks a provider up by id.
func (r *Registry) Get(id string) (Provider, error) {
	p, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (have %v)", id, r.IDs())
	}
	return p, nil
}

// IDs lists the known provider ids in a stable order.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// All returns every provider, ordered by id.
func (r *Registry) All() []Provider {
	out := make([]Provider, 0, len(r.byID))
	for _, id := range r.IDs() {
		out = append(out, r.byID[id])
	}
	return out
}

// Resolve turns a possibly empty list of ids into providers: empty means all.
func (r *Registry) Resolve(ids []string) ([]Provider, error) {
	if len(ids) == 0 {
		return r.All(), nil
	}
	out := make([]Provider, 0, len(ids))
	for _, id := range ids {
		p, err := r.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
