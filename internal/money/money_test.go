package money

import (
	"encoding/json"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		currency string
		want     int64
		wantCur  string
	}{
		{"2,19", "EUR", 219, "EUR"},
		{"2.19", "EUR", 219, "EUR"},
		{"-0,21", "EUR", -21, "EUR"},
		{"1 234,56", "PLN", 123456, "PLN"},
		{"1.234,56", "PLN", 123456, "PLN"},
		{"1,234.56", "USD", 123456, "USD"},
		{"12,00 zł", "", 1200, "PLN"},
		{"€ 3,50", "", 350, "EUR"},
		{"PLN 12", "", 1200, "PLN"},
		{"0", "EUR", 0, "EUR"},
		{"7", "EUR", 700, "EUR"},
		{"1 000", "PLN", 100000, "PLN"},
		{"2,5", "EUR", 250, "EUR"},
		// Three digits after the last separator are a thousands group, not
		// decimals: "1,234" and "1.234" both mean one thousand two hundred and
		// thirty-four, in the two conventions these shops print.
		{"2,199", "EUR", 219900, "EUR"},
		{"12.500", "PLN", 1250000, "PLN"},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in, tc.currency)
		if err != nil {
			t.Errorf("Parse(%q, %q) failed: %v", tc.in, tc.currency, err)
			continue
		}
		if got.Minor != tc.want {
			t.Errorf("Parse(%q, %q).Minor = %d, want %d", tc.in, tc.currency, got.Minor, tc.want)
		}
		if got.Currency != tc.wantCur {
			t.Errorf("Parse(%q, %q).Currency = %q, want %q", tc.in, tc.currency, got.Currency, tc.wantCur)
		}
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "brak", "-"} {
		if _, err := Parse(in, "EUR"); err == nil {
			t.Errorf("Parse(%q) should have failed", in)
		}
	}
}

func TestDecimal(t *testing.T) {
	cases := map[int64]string{
		0:      "0.00",
		5:      "0.05",
		219:    "2.19",
		-21:    "-0.21",
		123456: "1234.56",
		-5:     "-0.05",
	}
	for minor, want := range cases {
		if got := New(minor, "EUR").Decimal(); got != want {
			t.Errorf("New(%d).Decimal() = %q, want %q", minor, got, want)
		}
	}
}

func TestArithmeticIsExact(t *testing.T) {
	// A receipt whose lines are summed must land on the cent.
	total := New(0, "EUR")
	for i := 0; i < 3; i++ {
		total = total.Add(New(10, "EUR")) // 0.10 three times
	}
	if total.Decimal() != "0.30" {
		t.Fatalf("sum = %s, want 0.30", total.Decimal())
	}
	if got := New(219, "EUR").Sub(New(21, "EUR")); got.Decimal() != "1.98" {
		t.Fatalf("2.19-0.21 = %s, want 1.98", got.Decimal())
	}
}

func TestJSONRoundTrip(t *testing.T) {
	original := New(-1234, "PLN")
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"amount":"-12.34","minor":-1234,"currency":"PLN"}`; string(encoded) != want {
		t.Fatalf("encoded = %s, want %s", encoded, want)
	}

	var decoded Amount
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != original {
		t.Fatalf("decoded = %+v, want %+v", decoded, original)
	}

	// A bare string is accepted too, which is what raw store payloads look like.
	var fromString Amount
	if err := json.Unmarshal([]byte(`"2,19"`), &fromString); err != nil {
		t.Fatal(err)
	}
	if fromString.Minor != 219 {
		t.Fatalf("fromString.Minor = %d, want 219", fromString.Minor)
	}
}
