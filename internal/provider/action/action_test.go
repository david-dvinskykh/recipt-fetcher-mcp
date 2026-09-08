package action

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/httpx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/secret"
)

// The response shapes below mirror the two GraphQL operations the Action app
// actually sends (recovered from the APK).

const receiptListResponse = `{"data":{"receiptList":{"receipts":[
  {"id":"R-1","dateTime":"2026-07-15T17:04:00Z","store":{"name":"Action Haarlem","id":"42"},"price":{"currency":"EUR","total":12.47},"returningPeriod":{"returnable":true}},
  {"id":"R-2","dateTime":"2026-06-02T10:00:00Z","store":{"name":"Action Zwolle","id":"7"},"price":{"currency":"EUR","total":3.99},"returningPeriod":{"returnable":false}}
],"offset":null}}}`

const singleReceiptResponse = `{"data":{"receipt":{
  "receiptNumber":"0042","dateTime":"2026-07-15T17:04:00Z","store":{"address":"Grote Markt 1, Haarlem"},
  "price":{"currency":"EUR","total":12.47,"subTotal":12.97,"employeeDiscount":0,"otherDiscounts":0.50,
    "vat":{"total":{"vatAmount":2.16,"totalIncludingVat":12.47,"totalExcludingVat":10.31},
      "perPercentage":[{"percentage":"21","specification":{"vatAmount":2.16,"totalIncludingVat":12.47,"totalExcludingVat":10.31}}]}},
  "totalQuantity":2,"barcode":"123","qrCode":"456",
  "products":[
    {"code":"8712345678901","description":"Batterijen AA","totalPrice":6.98,"price":{"adjustedSalesPrice":3.49,"regularSalesPrice":3.49},"quantity":2},
    {"code":"8712345000000","description":"Doekjes","totalPrice":5.49,"price":{"adjustedSalesPrice":5.49,"regularSalesPrice":5.99},"quantity":1}
  ],
  "paymentMethods":["PIN"],"membershipId":"M-9","warrantyYears":0}}}`

// gqlServer returns a test gateway that answers each operationName from a map.
func gqlServer(t *testing.T, byOp map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("authorization = %q, want Bearer tok", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			OperationName string `json:"operationName"`
		}
		_ = json.Unmarshal(body, &req)
		resp, ok := byOp[req.OperationName]
		if !ok {
			http.Error(w, `{"errors":[{"message":"unknown op"}]}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
}

func newProvider(t *testing.T, fields map[string]string, endpoint string) *Provider {
	t.Helper()
	store, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "" {
		fields["endpoint"] = endpoint
	}
	if len(fields) > 0 {
		if err := store.Set(ID, fields); err != nil {
			t.Fatal(err)
		}
	}
	return New(httpx.New("test"), store)
}

func TestListParsesReceiptList(t *testing.T) {
	srv := gqlServer(t, map[string]string{"GetReceipts": receiptListResponse})
	defer srv.Close()
	p := newProvider(t, map[string]string{"token": "tok"}, srv.URL)

	got, err := p.List(context.Background(), provider.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d receipts, want 2", len(got))
	}
	if got[0].ID != "R-1" || got[0].PurchasedAt.Format("2006-01-02") != "2026-07-15" {
		t.Fatalf("newest-first order or id wrong: %+v", got[0])
	}
	if got[0].Total.Decimal() != "12.47" || got[0].Total.Currency != "EUR" {
		t.Fatalf("total = %s", got[0].Total)
	}
	if got[0].Store == nil || got[0].Store.Name != "Action Haarlem" {
		t.Fatalf("store = %+v", got[0].Store)
	}
	if got[0].ItemsComplete {
		t.Error("a list entry has no item lines")
	}
}

func TestGetParsesFullReceipt(t *testing.T) {
	srv := gqlServer(t, map[string]string{"GetSingleReceipt": singleReceiptResponse})
	defer srv.Close()
	p := newProvider(t, map[string]string{"token": "tok"}, srv.URL)

	r, err := p.Get(context.Background(), "R-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Number != "0042" || !r.ItemsComplete {
		t.Fatalf("receipt meta wrong: %+v", r)
	}
	if r.Total.Decimal() != "12.47" {
		t.Fatalf("total = %s", r.Total)
	}
	if r.Store == nil || r.Store.Street != "Grote Markt 1, Haarlem" {
		t.Fatalf("store = %+v", r.Store)
	}
	if len(r.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(r.Items))
	}
	batteries := r.Items[0]
	if batteries.Name != "Batterijen AA" || batteries.Quantity != 2 || batteries.Total.Decimal() != "6.98" {
		t.Errorf("first item = %+v", batteries)
	}
	// The second product is discounted 5.99 -> 5.49 per unit.
	wipes := r.Items[1]
	if len(wipes.Discounts) != 1 || wipes.Discounts[0].Amount.Decimal() != "0.50" {
		t.Errorf("expected a 0.50 reduction on the wipes: %+v", wipes)
	}
	if len(r.Taxes) != 1 || r.Taxes[0].Rate != "21" || r.Taxes[0].Amount.Decimal() != "2.16" {
		t.Errorf("taxes = %+v", r.Taxes)
	}
	if len(r.Payments) != 1 || r.Payments[0].Method != "PIN" {
		t.Errorf("payments = %+v", r.Payments)
	}
	if len(r.Discounts) != 1 || r.Discounts[0].Amount.Decimal() != "0.50" {
		t.Errorf("receipt-level otherDiscounts not mapped: %+v", r.Discounts)
	}
	if len(r.Raw) == 0 || !json.Valid(r.Raw) {
		t.Error("raw payload should be kept")
	}
}

func TestGraphQLAuthErrorIsCredentialProblem(t *testing.T) {
	// A 200 body carrying a GraphQL auth error must map to ErrCredentialsRejected
	// so the mailbox fallback can take over.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"Unauthorized: token expired"}]}`))
	}))
	defer srv.Close()
	p := newProvider(t, map[string]string{"token": "tok"}, srv.URL)

	_, err := p.List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrCredentialsRejected) {
		t.Fatalf("err = %v, want ErrCredentialsRejected", err)
	}
}

func TestListWithoutCredentials(t *testing.T) {
	p := newProvider(t, map[string]string{}, "")
	_, err := p.List(context.Background(), provider.Query{})
	if !errors.Is(err, provider.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
	if p.Status(context.Background()).LoggedIn {
		t.Error("no token means not logged in")
	}
}

func TestRefreshTokenExchange(t *testing.T) {
	// A provider with only a refresh token must call the Gigya token endpoint
	// once, then reuse the access token for the gateway.
	var refreshCalls int
	gateway := gqlServer(t, map[string]string{"GetReceipts": receiptListResponse})
	defer gateway.Close()
	gigya := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt" {
			t.Errorf("bad refresh form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600,"refresh_token":"rt2"}`))
	}))
	defer gigya.Close()

	store, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ID, map[string]string{"refresh_token": "rt", "endpoint": gateway.URL}); err != nil {
		t.Fatal(err)
	}
	p := New(httpx.New("test"), store)
	p.tokenEndpoint = gigya.URL // test seam

	if _, err := p.List(context.Background(), provider.Query{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.List(context.Background(), provider.Query{}); err != nil {
		t.Fatal(err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refreshed %d times, want the access token to be cached", refreshCalls)
	}
	// The rotated refresh token must have been persisted.
	if got := store.Field(ID, "refresh_token"); got != "rt2" {
		t.Errorf("rotated refresh token = %q, want rt2", got)
	}
}

func TestQueriesAreTheAppsOwn(t *testing.T) {
	// Guard against accidental edits to the operation text the app uses.
	if !strings.Contains(listQuery, "receiptList(limit: $limit, offset: $offset)") {
		t.Error("list query drifted from the app's GetReceipts")
	}
	if !strings.Contains(detailQuery, "receipt(id: $receiptId)") || !strings.Contains(detailQuery, "products {") {
		t.Error("detail query drifted from the app's GetSingleReceipt")
	}
}
