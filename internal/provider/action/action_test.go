package action

import (
	"context"
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
	obj := object(t, `{
	  "id": "R-1",
	  "date": "2026-07-15T17:04:00Z",
	  "total": {"amount": "12.47", "currency": "EUR"},
	  "store": {"name": "Action Haarlem", "id": "42", "city": "Haarlem"},
	  "lines": [
	    {"name": "Batterijen AA", "quantity": 2, "unitPrice": {"amount": "3.49", "currency": "EUR"}, "ean": "8712345678901"},
	    {"name": "Doekjes", "quantity": 1, "unitPrice": {"amount": "5.49", "currency": "EUR"}, "discount": {"amount": "0.50", "currency": "EUR"}}
	  ]
	}`)

	r := toReceipt(obj)
	if r.ID != "R-1" || r.Total.Decimal() != "12.47" || r.Currency != "EUR" {
		t.Fatalf("receipt = %+v", r)
	}
	if r.Store == nil || r.Store.Name != "Action Haarlem" || r.Store.City != "Haarlem" {
		t.Fatalf("store = %+v", r.Store)
	}
	if len(r.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(r.Items))
	}
	if got := r.Items[0]; got.Total.Decimal() != "6.98" || got.Code != "8712345678901" {
		t.Errorf("first item = %+v", got)
	}
	discounted := r.Items[1]
	if len(discounted.Discounts) != 1 || discounted.Gross.Decimal() != "5.99" || discounted.Total.Decimal() != "5.49" {
		t.Errorf("discounted item = %+v", discounted)
	}
}

func TestToReceiptWithFlatFields(t *testing.T) {
	// The same data in a flatter shape: field probing has to cope with both.
	obj := object(t, `{"receiptId":"R-2","transactionDate":"15-07-2026","totalAmount":"9,99","currency":"EUR","storeName":"Action Zwolle"}`)
	r := toReceipt(obj)
	if r.ID != "R-2" {
		t.Errorf("id = %q", r.ID)
	}
	if r.Total.Decimal() != "9.99" || r.Total.Currency != "EUR" {
		t.Errorf("total = %s", r.Total)
	}
	if r.PurchasedAt.Format("2006-01-02") != "2026-07-15" {
		t.Errorf("purchased at %s", r.PurchasedAt)
	}
	if r.Store.Name != "Action Zwolle" {
		t.Errorf("store = %+v", r.Store)
	}
	if r.ItemsComplete {
		t.Error("no lines in the payload means the items are not complete")
	}
}

func newTestProvider(t *testing.T, fields map[string]string) *Provider {
	t.Helper()
	store, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) > 0 {
		if err := store.Set(ID, fields); err != nil {
			t.Fatal(err)
		}
	}
	return New(httpx.New("test"), store)
}

func TestUnconfiguredProviderDefersToTheFallback(t *testing.T) {
	p := newTestProvider(t, nil)
	if status := p.Status(context.Background()); status.LoggedIn {
		t.Error("without an api_base the provider is not logged in")
	}
	_, err := p.List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported so the mailbox answers instead", err)
	}
}

func TestListWithBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/receipts" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"receipts":[{"id":"R-1","date":"2026-07-15T17:04:00Z","total":{"amount":"12.47","currency":"EUR"}}]}`))
	}))
	defer server.Close()

	p := newTestProvider(t, map[string]string{"api_base": server.URL, "token": "tok"})
	got, err := p.List(context.Background(), provider.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "R-1" {
		t.Fatalf("receipts = %+v", got)
	}
}

func TestLoginExchangesEmailAndPassword(t *testing.T) {
	var loginCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/login" {
			loginCalls++
			_, _ = w.Write([]byte(`{"access_token":"fresh"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("the token from the login was not used: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"receipts":[]}`))
	}))
	defer server.Close()

	p := newTestProvider(t, map[string]string{"api_base": server.URL, "email": "a@b.c", "password": "pw"})
	if _, err := p.List(context.Background(), provider.Query{}); err != nil {
		t.Fatal(err)
	}
	// The token is cached, so a second call does not log in again.
	if _, err := p.List(context.Background(), provider.Query{}); err != nil {
		t.Fatal(err)
	}
	if loginCalls != 1 {
		t.Fatalf("logged in %d times, want the token to be reused", loginCalls)
	}
}

func TestListReportsAMovedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	p := newTestProvider(t, map[string]string{"api_base": server.URL, "token": "tok"})
	_, err := p.List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrEndpointUnavailable) {
		t.Fatalf("err = %v, want ErrEndpointUnavailable", err)
	}
}
