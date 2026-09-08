package jsonx

import "testing"

func decode(t *testing.T, raw string) Object {
	t.Helper()
	obj, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestGetIsCaseInsensitiveAndOrdered(t *testing.T) {
	obj := decode(t, `{"TotalCost": 1, "total": 2}`)
	if got := obj.String("totalcost", "total"); got != "1" {
		t.Fatalf("got %q, want the first listed key to win", got)
	}
	if got := obj.String("missing", "total"); got != "2" {
		t.Fatalf("got %q, want the fallback key", got)
	}
}

func TestStringKeepsNumericPrecision(t *testing.T) {
	obj := decode(t, `{"amount": 12.30, "count": 3}`)
	// 12.30 decodes to the float 12.3; what matters is that it does not become
	// "12.300000000000001" or scientific notation on the way to the parser.
	if got := obj.String("amount"); got != "12.3" {
		t.Fatalf("amount = %q", got)
	}
	if got := obj.String("count"); got != "3" {
		t.Fatalf("count = %q", got)
	}
}

func TestFindArrayWalksEnvelopes(t *testing.T) {
	obj := decode(t, `{"data": {"page": {"orders": [{"id": "a"}, {"id": "b"}]}}}`)
	got, found := obj.FindArray("orders")
	if !found || len(got) != 2 {
		t.Fatalf("found=%v len=%d, want the nested array", found, len(got))
	}
	if got[1].String("id") != "b" {
		t.Fatalf("second element = %+v", got[1])
	}
}

func TestFindArrayDistinguishesEmptyFromMissing(t *testing.T) {
	empty := decode(t, `{"orders": []}`)
	got, found := empty.FindArray("orders")
	if !found {
		t.Fatal("an empty list is a valid answer, not a missing key")
	}
	if len(got) != 0 {
		t.Fatalf("len = %d", len(got))
	}

	missing := decode(t, `{"somethingElse": 1}`)
	if _, found := missing.FindArray("orders"); found {
		t.Fatal("a missing key must be reported as missing")
	}
}

func TestFloatAcceptsStrings(t *testing.T) {
	obj := decode(t, `{"quantity": "0,404", "count": 2, "name": "x"}`)
	if got, ok := obj.Float("quantity"); !ok || got != 0.404 {
		t.Errorf("quantity = %v (%v)", got, ok)
	}
	if got, ok := obj.Float("count"); !ok || got != 2 {
		t.Errorf("count = %v (%v)", got, ok)
	}
	if _, ok := obj.Float("name"); ok {
		t.Error("a non-numeric string must not parse")
	}
}

func TestTimeFormats(t *testing.T) {
	cases := map[string]string{
		`{"d": "2026-08-02T18:24:05Z"}`: "2026-08-02",
		`{"d": "2026-08-02 18:24:05"}`:  "2026-08-02",
		`{"d": "02.08.2026"}`:           "2026-08-02",
		`{"d": "02/08/2026"}`:           "2026-08-02",
		`{"d": 1785000000}`:             "2026-07-25",
		`{"d": 1785000000000}`:          "2026-07-25",
	}
	for raw, want := range cases {
		got := decode(t, raw).Time("d")
		if got.Format("2006-01-02") != want {
			t.Errorf("%s -> %s, want %s", raw, got, want)
		}
	}
	if got := decode(t, `{"d": "not a date"}`).Time("d"); !got.IsZero() {
		t.Errorf("unparsable date = %s, want the zero time", got)
	}
}

func TestAmountShapes(t *testing.T) {
	cases := []struct {
		raw      string
		want     string
		currency string
	}{
		{`{"total": {"amount": "149.99", "currency": "PLN"}}`, "149.99", "PLN"},
		{`{"total": {"value": 12.5, "currencyCode": "eur"}}`, "12.50", "EUR"},
		{`{"total": "12,30 zł"}`, "12.30", "PLN"},
		{`{"total": 9.99}`, "9.99", ""},
	}
	for _, tc := range cases {
		got, currency := Amount(decode(t, tc.raw), "total")
		if got.Decimal() != tc.want || currency != tc.currency {
			t.Errorf("%s -> %s/%q, want %s/%q", tc.raw, got.Decimal(), currency, tc.want, tc.currency)
		}
	}
	if got, _ := Amount(decode(t, `{"other": 1}`), "total"); !got.IsZero() {
		t.Error("a missing amount must come back as zero")
	}
}
