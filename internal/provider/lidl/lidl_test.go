package lidl

import (
	"encoding/json"
	"testing"
)

// ticketFixture is a Lidl Plus ticket detail cut down to the fields this
// provider reads, in the shapes the API uses: amounts as comma decimals,
// quantities as strings, discounts as their own list.
const ticketFixture = `{
  "id": "202603041230-1234-5678",
  "date": "2026-03-04T12:30:11",
  "totalAmount": "23,47",
  "sequenceNumber": "0042",
  "storeCode": "1234",
  "currency": {"code": "PLN", "symbol": "zł"},
  "store": {"id": "1234", "name": "Lidl Warszawa Wola", "street": "ul. Kasprzaka 1", "locality": "Warszawa", "postCode": "01-234", "countryCode": "PL"},
  "itemsLine": [
    {
      "name": "Vegane Frikadellen",
      "quantity": "1",
      "isWeight": false,
      "currentUnitPrice": "2,19",
      "originalAmount": "2,19",
      "taxGroup": "1",
      "taxGroupName": "A",
      "codeInput": "4023456245134",
      "deposit": null,
      "discounts": [{"description": "5€ Coupon", "amount": "0,21"}]
    },
    {
      "name": "Jabłka luz",
      "quantity": "0,404",
      "isWeight": true,
      "currentUnitPrice": "4,99",
      "originalAmount": "2,02",
      "taxGroupName": "B",
      "codeInput": "20123",
      "deposit": "0,50",
      "discounts": []
    }
  ],
  "taxes": [{"taxGroupName": "A", "percentage": "23", "amount": "0,37", "netAmount": "1,61"}],
  "couponsUsed": [{"description": "Kupon 5%", "amount": "1,00"}],
  "payments": [{"description": "Karta", "amount": "23,47"}]
}`

func TestTicketDetailMapping(t *testing.T) {
	var detail ticketDetail
	if err := json.Unmarshal([]byte(ticketFixture), &detail); err != nil {
		t.Fatal(err)
	}
	r := detail.toReceipt()

	if r.Provider != ID || r.ID != "202603041230-1234-5678" {
		t.Fatalf("identity wrong: %+v", r)
	}
	if r.Currency != "PLN" || r.Total.Decimal() != "23.47" {
		t.Fatalf("total = %s %s, want 23.47 PLN", r.Total.Decimal(), r.Currency)
	}
	if r.PurchasedAt.Format("2006-01-02 15:04") != "2026-03-04 12:30" {
		t.Fatalf("purchased at %s", r.PurchasedAt)
	}
	if !r.ItemsComplete {
		t.Error("a ticket detail carries the full item list")
	}
	if r.Store == nil || r.Store.Name != "Lidl Warszawa Wola" || r.Store.City != "01-234 Warszawa" {
		t.Fatalf("store = %+v", r.Store)
	}

	if len(r.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(r.Items))
	}

	first := r.Items[0]
	if first.Gross.Decimal() != "2.19" {
		t.Errorf("gross = %s, want 2.19", first.Gross.Decimal())
	}
	if first.Total.Decimal() != "1.98" {
		t.Errorf("total after the 0.21 discount = %s, want 1.98", first.Total.Decimal())
	}
	if len(first.Discounts) != 1 || first.Discounts[0].Amount.Decimal() != "0.21" {
		t.Errorf("discounts = %+v", first.Discounts)
	}
	if first.Code != "4023456245134" || first.TaxGroup != "A" {
		t.Errorf("code/tax group = %q/%q", first.Code, first.TaxGroup)
	}

	second := r.Items[1]
	if !second.IsWeight || second.Quantity != 0.404 {
		t.Errorf("weight line = %+v", second)
	}
	if second.Deposit == nil || second.Deposit.Decimal() != "0.50" {
		t.Errorf("deposit = %+v", second.Deposit)
	}

	if len(r.Taxes) != 1 || r.Taxes[0].Amount.Decimal() != "0.37" || r.Taxes[0].Net.Decimal() != "1.61" {
		t.Errorf("taxes = %+v", r.Taxes)
	}
	if len(r.Discounts) != 1 || r.Discounts[0].Amount.Decimal() != "1.00" {
		t.Errorf("receipt discounts = %+v", r.Discounts)
	}
	if len(r.Payments) != 1 || r.Payments[0].Method != "Karta" {
		t.Errorf("payments = %+v", r.Payments)
	}
}

func TestTicketSummaryMapping(t *testing.T) {
	const fixture = `{"id":"abc","date":"2026-03-04T12:30:11","totalAmount":"23,47","storeCode":"1234","currency":{"code":"PLN"},"articlesCount":7}`
	var summary ticketSummary
	if err := json.Unmarshal([]byte(fixture), &summary); err != nil {
		t.Fatal(err)
	}
	r := summary.toReceipt()

	if r.Total.Decimal() != "23.47" || r.Total.Currency != "PLN" {
		t.Fatalf("total = %s", r.Total)
	}
	if r.ItemsComplete {
		t.Error("a listing entry has no item lines, so ItemsComplete must be false")
	}
	if r.Store == nil || r.Store.Code != "1234" {
		t.Fatalf("store = %+v", r.Store)
	}
	if len(r.Notes) == 0 {
		t.Error("the article count should be reported as a note")
	}
}

func TestTicketPageHasMore(t *testing.T) {
	page := ticketPage{Size: 10, TotalCount: 25}
	if !page.hasMore(1) || !page.hasMore(2) {
		t.Error("pages 1 and 2 of 25 items are not the last")
	}
	if page.hasMore(3) {
		t.Error("page 3 of 25 items is the last")
	}
	if (ticketPage{}).hasMore(1) {
		t.Error("an empty page cannot claim more")
	}
}

func TestParseQuantity(t *testing.T) {
	cases := map[string]float64{"1": 1, "0,404": 0.404, "2.000": 2, "": 0, "x": 0}
	for in, want := range cases {
		if got := parseQuantity(in); got != want {
			t.Errorf("parseQuantity(%q) = %v, want %v", in, got, want)
		}
	}
}
