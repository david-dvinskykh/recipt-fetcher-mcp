package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
)

// fake is a provider whose answers the test dictates.
type fake struct {
	id       string
	receipts []receipt.Receipt
	listErr  error
	getErr   error
	calls    int
}

func (f *fake) ID() string          { return f.id }
func (f *fake) DisplayName() string { return f.id }
func (f *fake) Status(context.Context) Status {
	return Status{Provider: f.id, LoggedIn: f.listErr == nil}
}
func (f *fake) Login(context.Context, map[string]string) (LoginResult, error) {
	return LoginResult{Provider: f.id, OK: true}, nil
}
func (f *fake) Logout(context.Context) error { return nil }

func (f *fake) List(context.Context, Query) ([]receipt.Receipt, error) {
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.receipts, nil
}

func (f *fake) Get(_ context.Context, id string) (receipt.Receipt, error) {
	if f.getErr != nil {
		return receipt.Receipt{}, f.getErr
	}
	for _, r := range f.receipts {
		if r.ID == id {
			return r, nil
		}
	}
	return receipt.Receipt{}, errors.New("not found")
}

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestFallbackMergesBothSources(t *testing.T) {
	primary := &fake{id: "allegro", receipts: []receipt.Receipt{
		{Provider: "allegro", ID: "order-1", Source: receipt.SourceAPI, PurchasedAt: at("2026-08-02"), Total: money.New(1000, "PLN")},
	}}
	secondary := &fake{id: "allegro", receipts: []receipt.Receipt{
		// Same purchase, seen in the mail: must not appear twice.
		{Provider: "allegro", ID: "mail-x", Source: receipt.SourceEmail, PurchasedAt: at("2026-08-02"), Total: money.New(1000, "PLN")},
		// A purchase the API did not return: must survive.
		{Provider: "allegro", ID: "mail-y", Source: receipt.SourceEmail, PurchasedAt: at("2026-08-05"), Total: money.New(2500, "PLN")},
	}}

	got, err := NewFallback(primary, secondary).List(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d receipts, want 2:\n%+v", len(got), got)
	}
	if got[0].ID != "mail-y" {
		t.Errorf("newest first expected, got %q", got[0].ID)
	}
}

func TestFallbackUsesMailWhenAPIIsNotLoggedIn(t *testing.T) {
	primary := &fake{id: "action", listErr: ErrNotLoggedIn}
	secondary := &fake{id: "action", receipts: []receipt.Receipt{
		{Provider: "action", ID: "mail-1", Source: receipt.SourceEmail, PurchasedAt: at("2026-08-02"), Total: money.New(999, "EUR")},
	}}

	got, err := NewFallback(primary, secondary).List(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d receipts, want the mail-derived one", len(got))
	}
	if len(got[0].Notes) == 0 || !strings.Contains(strings.Join(got[0].Notes, " "), "e-mail") {
		t.Errorf("the caller must be told the data came from e-mail; notes = %v", got[0].Notes)
	}
}

func TestFallbackDoesNotHideRealFailures(t *testing.T) {
	// A cancelled context is not something the mailbox can cover for.
	primary := &fake{id: "action", listErr: context.Canceled}
	secondary := &fake{id: "action", receipts: []receipt.Receipt{{Provider: "action", ID: "mail-1"}}}

	if _, err := NewFallback(primary, secondary).List(context.Background(), Query{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the original failure", err)
	}
	if secondary.calls != 0 {
		t.Error("the fallback should not have been consulted")
	}
}

func TestFallbackReportsBothErrors(t *testing.T) {
	primary := &fake{id: "action", listErr: ErrCredentialsRejected}
	secondary := &fake{id: "action", listErr: ErrNotLoggedIn}

	_, err := NewFallback(primary, secondary).List(context.Background(), Query{})
	if err == nil {
		t.Fatal("want an error when neither source works")
	}
	if !strings.Contains(err.Error(), "mailbox fallback") {
		t.Errorf("error should name both sources, got %v", err)
	}
}

func TestFallbackWithoutSecondaryIsThePrimary(t *testing.T) {
	primary := &fake{id: "lidl"}
	if got := NewFallback(primary, nil); got != Provider(primary) {
		t.Fatal("a nil fallback must leave the provider untouched")
	}
}

func TestQueryNormalize(t *testing.T) {
	if got := (Query{}).Normalize().Limit; got != 50 {
		t.Errorf("default limit = %d, want 50", got)
	}
	if got := (Query{Limit: 10000}).Normalize().Limit; got != 500 {
		t.Errorf("limit is not capped: %d", got)
	}
}

func TestRegistryResolve(t *testing.T) {
	registry := NewRegistry(&fake{id: "lidl"}, &fake{id: "action"})
	all, err := registry.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("empty selection should mean every provider, got %d", len(all))
	}
	if _, err := registry.Resolve([]string{"tesco"}); err == nil {
		t.Fatal("an unknown provider must be reported")
	}
}
