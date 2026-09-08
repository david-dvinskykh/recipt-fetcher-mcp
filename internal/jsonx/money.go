package jsonx

import (
	"strings"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
)

// Amount reads a monetary value that may be a nested {amount, currency} object,
// a bare number, or a formatted string such as "12,30 zł". It returns the
// parsed amount and the currency it managed to determine (possibly empty).
func Amount(obj Object, keys ...string) (money.Amount, string) {
	value, ok := obj.Get(keys...)
	if !ok {
		return money.Amount{}, ""
	}
	switch typed := value.(type) {
	case map[string]any:
		nested := Object(typed)
		currency := strings.ToUpper(nested.String("currency", "currencyCode", "code"))
		amount, err := money.Parse(nested.String("amount", "value", "gross", "total", "price"), currency)
		if err != nil {
			return money.New(0, currency), currency
		}
		return amount, amount.Currency
	default:
		amount, err := money.Parse(AsString(value), "")
		if err != nil {
			return money.Amount{}, ""
		}
		return amount, amount.Currency
	}
}
