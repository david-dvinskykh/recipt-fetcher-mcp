package receipt

import (
	"strings"
	"testing"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDedupKeepsFirst(t *testing.T) {
	receipts := []Receipt{
		{Provider: "lidl", ID: "1", Source: SourceAPI},
		{Provider: "lidl", ID: "1", Source: SourceEmail},
		{Provider: "lidl", ID: "2", Source: SourceAPI},
	}
	got := Dedup(receipts)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Source != SourceAPI {
		t.Fatalf("first receipt source = %q, want the api one to win", got[0].Source)
	}
}

func TestDropOverlappingMatchesOnDayAndAmount(t *testing.T) {
	primary := []Receipt{{
		Provider:    "allegro",
		ID:          "order-1",
		PurchasedAt: day("2026-08-01").Add(9 * time.Hour),
		Total:       money.New(4599, "PLN"),
	}}
	// The same purchase from the mailbox, with no order number in the mail.
	extra := []Receipt{
		{
			Provider:    "allegro",
			ID:          "mail-abcdef",
			PurchasedAt: day("2026-08-01").Add(21 * time.Hour),
			Total:       money.New(4599, "PLN"),
		},
		{
			Provider:    "allegro",
			ID:          "mail-123456",
			PurchasedAt: day("2026-08-02"),
			Total:       money.New(1000, "PLN"),
		},
	}

	got := DropOverlapping(primary, extra)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (the duplicate should be dropped)", len(got))
	}
	if got[0].ID != "mail-123456" {
		t.Fatalf("kept %q, want the receipt that is not a duplicate", got[0].ID)
	}
}

func TestSortByDateNewestFirst(t *testing.T) {
	receipts := []Receipt{
		{Provider: "a", ID: "old", PurchasedAt: day("2026-01-01")},
		{Provider: "a", ID: "new", PurchasedAt: day("2026-06-01")},
	}
	SortByDate(receipts)
	if receipts[0].ID != "new" {
		t.Fatalf("first = %q, want newest first", receipts[0].ID)
	}
}

func TestInRange(t *testing.T) {
	r := Receipt{PurchasedAt: day("2026-05-15")}
	if !r.InRange(day("2026-05-01"), day("2026-05-31")) {
		t.Error("receipt should be inside the range")
	}
	if r.InRange(day("2026-06-01"), time.Time{}) {
		t.Error("receipt before an open-ended range should be outside")
	}
	if !r.InRange(time.Time{}, time.Time{}) {
		t.Error("an unbounded range should hold every receipt")
	}
}

func TestEncodeCSVOneRowPerItem(t *testing.T) {
	receipts := []Receipt{{
		Provider:    "lidl",
		ID:          "t1",
		Source:      SourceAPI,
		PurchasedAt: day("2026-03-04").Add(12*time.Hour + 30*time.Minute),
		Store:       &Store{Name: "Lidl 1234", City: "Warszawa"},
		Total:       money.New(500, "PLN"),
		Items: []Item{
			{Name: "Mleko", Quantity: 2, UnitPrice: money.New(200, "PLN"), Gross: money.New(400, "PLN"), Total: money.New(350, "PLN")},
			{Name: "Chleb", Quantity: 1, UnitPrice: money.New(150, "PLN"), Gross: money.New(150, "PLN"), Total: money.New(150, "PLN")},
		},
		ItemsComplete: true,
	}}

	data, err := Encode(receipts, FormatCSV)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header + 2 items:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[1], "Mleko") || !strings.Contains(lines[1], "0.50") {
		t.Errorf("first item row lost data: %s", lines[1])
	}
	if !strings.Contains(lines[1], "Lidl 1234") {
		t.Errorf("receipt columns are not repeated on the item row: %s", lines[1])
	}
}

func TestEncodeCSVKeepsReceiptsWithoutItems(t *testing.T) {
	receipts := []Receipt{{
		Provider:    "allegro",
		ID:          "order-9",
		Source:      SourceEmail,
		PurchasedAt: day("2026-03-04"),
		Total:       money.New(12345, "PLN"),
	}}
	data, err := Encode(receipts, FormatCSV)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want header + the itemless receipt:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[1], "123.45") {
		t.Errorf("row lost the total: %s", lines[1])
	}
}

func TestParseFormat(t *testing.T) {
	for _, in := range []string{"", "json", "JSON", "csv", "ndjson"} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q) failed: %v", in, err)
		}
	}
	if _, err := ParseFormat("xlsx"); err == nil {
		t.Error("ParseFormat(\"xlsx\") should have failed")
	}
}
