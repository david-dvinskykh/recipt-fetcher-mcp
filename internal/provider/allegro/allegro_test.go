package allegro

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/httpx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/jsonx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/secret"
)

func object(t *testing.T, raw string) jsonx.Object {
	t.Helper()
	obj, err := jsonx.Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestToReceipt(t *testing.T) {
	order := object(t, `{
	  "id": "6f2f3f4e-1111",
	  "boughtAt": "2026-08-02T18:24:05Z",
	  "totalCost": {"amount": "149.99", "currency": "PLN"},
	  "seller": {"login": "SklepXYZ"},
	  "offers": [
	    {"id": "12345", "name": "Klocki konstrukcyjne", "quantity": 2, "price": {"amount": "60.00", "currency": "PLN"}, "category": {"name": "Zabawki"}},
	    {"id": "67890", "title": "Bateria AA", "quantity": 1, "price": {"amount": "29.99", "currency": "PLN"}}
	  ]
	}`)

	r := toReceipt(order)
	if r.ID != "6f2f3f4e-1111" || r.Provider != ID {
		t.Fatalf("identity = %s/%s", r.Provider, r.ID)
	}
	if r.Total.Decimal() != "149.99" || r.Currency != "PLN" {
		t.Fatalf("total = %s", r.Total)
	}
	if r.PurchasedAt.Format("2006-01-02") != "2026-08-02" {
		t.Fatalf("purchased at %s", r.PurchasedAt)
	}
	if r.Store == nil || r.Store.Name != "SklepXYZ" {
		t.Fatalf("store = %+v", r.Store)
	}
	if len(r.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(r.Items))
	}
	if got := r.Items[0]; got.Total.Decimal() != "120.00" || got.Category != "Zabawki" || got.Seller != "SklepXYZ" {
		t.Errorf("first item = %+v", got)
	}
	if !r.ItemsComplete {
		t.Error("an order with offers has its lines")
	}
}

func TestToReceiptSumsWhenTotalIsMissing(t *testing.T) {
	order := object(t, `{
	  "id": "x",
	  "date": "2026-08-02",
	  "offers": [{"name": "Rzecz", "quantity": 3, "price": {"amount": "10.00", "currency": "PLN"}}]
	}`)

	r := toReceipt(order)
	if r.Total.Decimal() != "30.00" {
		t.Fatalf("total = %s, want the summed lines", r.Total)
	}
	if len(r.Notes) == 0 {
		t.Error("summing instead of reading the total is a caveat worth reporting")
	}
}

func TestToReceiptPrefersExplicitLineTotal(t *testing.T) {
	order := object(t, `{
	  "id": "x",
	  "offers": [{"name": "Rzecz", "quantity": 3, "price": {"amount": "10.00", "currency": "PLN"}, "totalPrice": {"amount": "25.00", "currency": "PLN"}}]
	}`)
	r := toReceipt(order)
	if r.Items[0].Total.Decimal() != "25.00" {
		t.Fatalf("line total = %s, want the explicit 25.00", r.Items[0].Total)
	}
}

func newTestProvider(t *testing.T, endpoint string) *Provider {
	t.Helper()
	store, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ID, map[string]string{"cookie": "session=1", "endpoint": endpoint}); err != nil {
		t.Fatal(err)
	}
	return New(httpx.New("test"), store)
}

func TestListReadsTheOrderArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "session=1" {
			t.Errorf("cookie not sent: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"myorders":[{"id":"a","boughtAt":"2026-08-02T10:00:00Z","totalCost":{"amount":"10.00","currency":"PLN"}}]}`))
	}))
	defer server.Close()

	got, err := newTestProvider(t, server.URL).List(context.Background(), provider.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("receipts = %+v", got)
	}
}

func TestListDetectsALoginPage(t *testing.T) {
	// An expired session answers 200 with the login HTML.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>Zaloguj się</body></html>"))
	}))
	defer server.Close()

	_, err := newTestProvider(t, server.URL).List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrCredentialsRejected) {
		t.Fatalf("err = %v, want ErrCredentialsRejected so the mailbox takes over", err)
	}
}

func TestListReportsAChangedShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"somethingElse": {"nested": 1}}`))
	}))
	defer server.Close()

	_, err := newTestProvider(t, server.URL).List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrEndpointUnavailable) {
		t.Fatalf("err = %v, want ErrEndpointUnavailable", err)
	}
}

func TestListWithoutCookie(t *testing.T) {
	store, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(httpx.New("test"), store).List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestGetReturnsRawPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"myorders":[{"id":"a","boughtAt":"2026-08-02T10:00:00Z","totalCost":{"amount":"10.00","currency":"PLN"}}]}`))
	}))
	defer server.Close()

	got, err := newTestProvider(t, server.URL).Get(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Raw) == 0 || !json.Valid(got.Raw) {
		t.Fatalf("raw payload = %q", got.Raw)
	}
}
