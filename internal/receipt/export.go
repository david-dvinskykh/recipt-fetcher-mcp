package receipt

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Format is an export encoding.
type Format string

const (
	// FormatJSON writes one indented JSON array of receipts.
	FormatJSON Format = "json"
	// FormatNDJSON writes one receipt per line, which streams well into other tools.
	FormatNDJSON Format = "ndjson"
	// FormatCSV writes one row per item, with the receipt fields repeated.
	FormatCSV Format = "csv"
)

// ParseFormat validates a user supplied format name.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case "", FormatJSON:
		return FormatJSON, nil
	case FormatNDJSON:
		return FormatNDJSON, nil
	case FormatCSV:
		return FormatCSV, nil
	default:
		return "", fmt.Errorf("unknown format %q (want json, ndjson or csv)", s)
	}
}

// csvHeader is the column order of the CSV export. One row per item; a receipt
// with no items still produces a single row with empty item columns, so no
// purchase silently disappears from the export.
var csvHeader = []string{
	"provider", "receipt_id", "source", "purchased_at", "store", "store_city",
	"receipt_number", "receipt_total", "currency", "items_complete",
	"item_name", "item_code", "item_category", "item_quantity", "item_unit",
	"item_unit_price", "item_gross", "item_total", "item_discount", "item_tax_group",
	"item_seller",
}

// Encode renders receipts in the requested format.
func Encode(rs []Receipt, format Format) ([]byte, error) {
	switch format {
	case FormatJSON:
		return json.MarshalIndent(rs, "", "  ")
	case FormatNDJSON:
		var b strings.Builder
		enc := json.NewEncoder(&b)
		for _, r := range rs {
			if err := enc.Encode(r); err != nil {
				return nil, err
			}
		}
		return []byte(b.String()), nil
	case FormatCSV:
		return encodeCSV(rs)
	default:
		return nil, fmt.Errorf("unknown format %q", format)
	}
}

func encodeCSV(rs []Receipt) ([]byte, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write(csvHeader); err != nil {
		return nil, err
	}
	for _, r := range rs {
		base := receiptColumns(r)
		if len(r.Items) == 0 {
			if err := w.Write(append(base, make([]string, len(csvHeader)-len(base))...)); err != nil {
				return nil, err
			}
			continue
		}
		for _, item := range r.Items {
			row := append(append([]string{}, base...), itemColumns(item)...)
			if err := w.Write(row); err != nil {
				return nil, err
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func receiptColumns(r Receipt) []string {
	store, city := "", ""
	if r.Store != nil {
		store, city = r.Store.Name, r.Store.City
	}
	return []string{
		r.Provider,
		r.ID,
		string(r.Source),
		r.PurchasedAt.Format("2006-01-02 15:04:05"),
		store,
		city,
		r.Number,
		r.Total.Decimal(),
		r.Total.Currency,
		strconv.FormatBool(r.ItemsComplete),
	}
}

func itemColumns(item Item) []string {
	discount := item.Gross.Sub(item.Total)
	return []string{
		item.Name,
		item.Code,
		item.Category,
		strconv.FormatFloat(item.Quantity, 'f', -1, 64),
		item.Unit,
		item.UnitPrice.Decimal(),
		item.Gross.Decimal(),
		item.Total.Decimal(),
		discount.Decimal(),
		item.TaxGroup,
		item.Seller,
	}
}
