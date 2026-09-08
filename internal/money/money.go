// Package money holds exact monetary amounts as integer minor units.
//
// Every currency handled by this server (EUR, PLN, CZK, ...) has two decimal
// places, so a single fixed exponent of 2 is enough. Amounts are never stored
// as floats: receipt totals have to add up to the cent.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Exponent is the number of decimal places used for all supported currencies.
const Exponent = 2

// ErrEmpty is returned by Parse when the input holds no digits at all.
var ErrEmpty = errors.New("money: empty amount")

// Amount is a monetary value in minor units (cents) of Currency.
// The zero Amount is a valid "0.00" with an unknown currency.
type Amount struct {
	Minor    int64
	Currency string
}

// New builds an Amount from minor units.
func New(minor int64, currency string) Amount {
	return Amount{Minor: minor, Currency: strings.ToUpper(strings.TrimSpace(currency))}
}

// Parse reads a human formatted amount such as "2,19", "-0.21", "1 234,56",
// "€2.19" or "12,00 zł". Both the comma and the dot are accepted as the decimal
// separator; spaces, apostrophes and the separator that is not the decimal one
// are treated as grouping characters.
//
// currency is used when the string carries no currency of its own; a currency
// found in the string wins.
func Parse(s, currency string) (Amount, error) {
	cleaned, found := splitCurrency(s)
	if found != "" {
		currency = found
	}
	minor, err := parseMinor(cleaned)
	if err != nil {
		return Amount{}, err
	}
	return New(minor, currency), nil
}

// ParseOrZero is Parse for inputs that are allowed to be missing: an empty or
// unparsable string yields a zero amount instead of an error.
func ParseOrZero(s, currency string) Amount {
	a, err := Parse(s, currency)
	if err != nil {
		return New(0, currency)
	}
	return a
}

// FromFloat converts a float (a quantity times a unit price, say) to minor
// units, rounding half away from zero.
func FromFloat(f float64, currency string) Amount {
	scaled := f * 100
	if scaled >= 0 {
		return New(int64(scaled+0.5), currency)
	}
	return New(int64(scaled-0.5), currency)
}

// Add returns a+b. The currency of a wins unless a has none.
func (a Amount) Add(b Amount) Amount {
	cur := a.Currency
	if cur == "" {
		cur = b.Currency
	}
	return New(a.Minor+b.Minor, cur)
}

// Sub returns a-b.
func (a Amount) Sub(b Amount) Amount { return a.Add(b.Neg()) }

// Neg returns -a.
func (a Amount) Neg() Amount { return New(-a.Minor, a.Currency) }

// Abs returns |a|.
func (a Amount) Abs() Amount {
	if a.Minor < 0 {
		return a.Neg()
	}
	return a
}

// IsZero reports whether the amount is exactly zero.
func (a Amount) IsZero() bool { return a.Minor == 0 }

// Decimal renders the amount as a plain decimal string with a dot separator
// and no currency, e.g. "-0.21".
func (a Amount) Decimal() string {
	sign := ""
	minor := a.Minor
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// String renders amount and currency, e.g. "2.19 EUR".
func (a Amount) String() string {
	if a.Currency == "" {
		return a.Decimal()
	}
	return a.Decimal() + " " + a.Currency
}

// jsonAmount is the wire shape: a decimal string for humans and models, the
// exact minor units for arithmetic, and the currency.
type jsonAmount struct {
	Amount   string `json:"amount"`
	Minor    int64  `json:"minor"`
	Currency string `json:"currency,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonAmount{Amount: a.Decimal(), Minor: a.Minor, Currency: a.Currency})
}

// UnmarshalJSON implements json.Unmarshaler. It accepts both the object shape
// produced by MarshalJSON and a bare string such as "2,19".
func (a *Amount) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*a = Amount{}
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := Parse(s, "")
		if err != nil {
			return err
		}
		*a = parsed
		return nil
	}
	var obj jsonAmount
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	if obj.Minor != 0 || obj.Amount == "" {
		*a = New(obj.Minor, obj.Currency)
		return nil
	}
	parsed, err := Parse(obj.Amount, obj.Currency)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// currencySymbols maps the symbols seen on receipts to ISO 4217 codes.
var currencySymbols = map[string]string{
	"€":   "EUR",
	"zł":  "PLN",
	"PLN": "PLN",
	"EUR": "EUR",
	"CZK": "CZK",
	"Kč":  "CZK",
	"$":   "USD",
	"£":   "GBP",
	"USD": "USD",
	"GBP": "GBP",
	"CHF": "CHF",
	"HUF": "HUF",
	"Ft":  "HUF",
	"RON": "RON",
	"BGN": "BGN",
	"SEK": "SEK",
	"DKK": "DKK",
	"NOK": "NOK",
	"UAH": "UAH",
	"₴":   "UAH",
}

// splitCurrency strips a currency symbol or code off the amount and returns
// the remainder together with the ISO code it found (empty when none).
func splitCurrency(s string) (rest, currency string) {
	rest = strings.TrimSpace(s)
	for symbol, code := range currencySymbols {
		idx := strings.Index(strings.ToLower(rest), strings.ToLower(symbol))
		if idx < 0 {
			continue
		}
		// A three letter code must not be cut out of a longer word.
		if len(symbol) == 3 && isLetterBoundaryViolated(rest, idx, len(symbol)) {
			continue
		}
		rest = rest[:idx] + rest[idx+len(symbol):]
		currency = code
		break
	}
	return strings.TrimSpace(rest), currency
}

func isLetterBoundaryViolated(s string, idx, length int) bool {
	before := idx - 1
	after := idx + length
	if before >= 0 && unicode.IsLetter(rune(s[before])) {
		return true
	}
	if after < len(s) && unicode.IsLetter(rune(s[after])) {
		return true
	}
	return false
}

// parseMinor turns a cleaned numeric string into minor units.
func parseMinor(s string) (int64, error) {
	negative := false
	var digits []rune
	lastSeparator := -1
	lastSeparatorRune := rune(0)

	for _, r := range s {
		switch {
		case r == '-' || r == '−': // ASCII hyphen or Unicode minus
			negative = true
		case r == '(': // accounting style negative
			negative = true
		case unicode.IsDigit(r):
			digits = append(digits, r)
		case r == ',' || r == '.':
			lastSeparator = len(digits)
			lastSeparatorRune = r
		default:
			// spaces, apostrophes, NBSP and stray letters are grouping noise
		}
	}
	if len(digits) == 0 {
		return 0, ErrEmpty
	}

	// Digits after the last separator decide whether it was a decimal point:
	// exactly one or two mean decimals, anything else means grouping.
	decimals := 0
	if lastSeparator >= 0 {
		trailing := len(digits) - lastSeparator
		if trailing == 1 || trailing == 2 {
			decimals = trailing
		} else if trailing == 3 && lastSeparatorRune == ',' && lastSeparator == 0 {
			// ",123" with nothing in front is not a grouping separator
			decimals = 3
		}
	}

	whole := string(digits[:len(digits)-decimals])
	frac := string(digits[len(digits)-decimals:])
	if whole == "" {
		whole = "0"
	}
	for len(frac) < Exponent {
		frac += "0"
	}
	roundUp := false
	if len(frac) > Exponent {
		if frac[Exponent] >= '5' {
			roundUp = true
		}
		frac = frac[:Exponent]
	}

	minor, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: %q: %w", s, err)
	}
	if roundUp {
		minor++
	}
	if negative {
		minor = -minor
	}
	return minor, nil
}
