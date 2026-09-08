// Package receipt defines the normalized shape every provider maps onto.
//
// The model is deliberately store agnostic: whoever categorizes the spending
// downstream should never have to know whether a line came from a Lidl ticket,
// an Action receipt or an Allegro order.
package receipt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
)

// Source says where the data came from, which tells the caller how much to
// trust the line level detail.
type Source string

const (
	// SourceAPI means the store's own API returned the receipt.
	SourceAPI Source = "api"
	// SourceEmail means the receipt was reconstructed from a confirmation mail,
	// so item level detail may be partial or missing.
	SourceEmail Source = "email"
)

// Store is where the purchase happened. Online orders carry only Name.
type Store struct {
	Name    string `json:"name,omitempty"`
	Code    string `json:"code,omitempty"`
	Street  string `json:"street,omitempty"`
	City    string `json:"city,omitempty"`
	Country string `json:"country,omitempty"`
}

// Discount is a reduction applied to a single item or to the whole receipt.
type Discount struct {
	Description string       `json:"description,omitempty"`
	Amount      money.Amount `json:"amount"`
}

// Tax is one VAT group of a receipt.
type Tax struct {
	Group   string       `json:"group,omitempty"`
	Rate    string       `json:"rate,omitempty"`
	Net     money.Amount `json:"net"`
	Amount  money.Amount `json:"amount"`
	Gross   money.Amount `json:"gross"`
	Comment string       `json:"comment,omitempty"`
}

// Payment is one payment leg of a receipt (card, cash, gift card, ...).
type Payment struct {
	Method string       `json:"method,omitempty"`
	Amount money.Amount `json:"amount"`
}

// Item is a single line of a receipt.
type Item struct {
	Name string `json:"name"`
	// Quantity is a piece count, or a weight/volume when IsWeight is set.
	Quantity float64 `json:"quantity"`
	IsWeight bool    `json:"is_weight,omitempty"`
	Unit     string  `json:"unit,omitempty"`
	// UnitPrice is the price of one piece or of one unit of weight.
	UnitPrice money.Amount `json:"unit_price"`
	// Gross is what the line cost before its own discounts.
	Gross money.Amount `json:"gross"`
	// Total is what the line actually cost after its discounts.
	Total money.Amount `json:"total"`
	// Discounts are the reductions already reflected in Total.
	Discounts []Discount `json:"discounts,omitempty"`
	// Code is the barcode, article number or offer id, whichever the store gives.
	Code string `json:"code,omitempty"`
	// TaxGroup is the store's VAT group label for this line.
	TaxGroup string `json:"tax_group,omitempty"`
	// Category is a hint from the store (Allegro sends one); never a guess of ours.
	Category string `json:"category,omitempty"`
	// Deposit is a bottle/packaging deposit charged on top of the line.
	Deposit *money.Amount `json:"deposit,omitempty"`
	// Seller is the marketplace seller, for platforms that have several.
	Seller string `json:"seller,omitempty"`
}

// Receipt is one purchase, normalized across providers.
type Receipt struct {
	Provider    string    `json:"provider"`
	ID          string    `json:"id"`
	Source      Source    `json:"source"`
	PurchasedAt time.Time `json:"purchased_at"`
	Currency    string    `json:"currency,omitempty"`

	Total     money.Amount `json:"total"`
	Store     *Store       `json:"store,omitempty"`
	Number    string       `json:"number,omitempty"`
	Items     []Item       `json:"items,omitempty"`
	Taxes     []Tax        `json:"taxes,omitempty"`
	Discounts []Discount   `json:"discounts,omitempty"`
	Payments  []Payment    `json:"payments,omitempty"`

	// ItemsComplete is false when the source could not give a full line listing,
	// which happens with most confirmation mails.
	ItemsComplete bool `json:"items_complete"`
	// Notes carry caveats worth showing to whoever categorizes the receipt.
	Notes []string `json:"notes,omitempty"`
	// Link points at the receipt in the store's own web UI, when there is one.
	Link string `json:"link,omitempty"`
	// Raw is the untouched provider payload; only filled in on explicit request.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Key is a stable identity for deduplicating receipts across sources.
func (r Receipt) Key() string {
	return fmt.Sprintf("%s:%s", r.Provider, r.ID)
}

// Summary is the compact form returned by listings: enough to decide which
// receipts are worth fetching in full, without flooding the context.
type Summary struct {
	Provider    string       `json:"provider"`
	ID          string       `json:"id"`
	Source      Source       `json:"source"`
	PurchasedAt time.Time    `json:"purchased_at"`
	Total       money.Amount `json:"total"`
	Store       string       `json:"store,omitempty"`
	ItemCount   int          `json:"item_count"`
	Number      string       `json:"number,omitempty"`
	Notes       []string     `json:"notes,omitempty"`
}

// Summarize reduces a receipt to its listing form.
func (r Receipt) Summarize() Summary {
	s := Summary{
		Provider:    r.Provider,
		ID:          r.ID,
		Source:      r.Source,
		PurchasedAt: r.PurchasedAt,
		Total:       r.Total,
		ItemCount:   len(r.Items),
		Number:      r.Number,
		Notes:       r.Notes,
	}
	if r.Store != nil {
		s.Store = strings.TrimSpace(strings.Join(nonEmpty(r.Store.Name, r.Store.City), ", "))
	}
	return s
}

// SortByDate orders receipts newest first, breaking ties on the key so the
// output of two identical runs is identical.
func SortByDate(rs []Receipt) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].PurchasedAt.Equal(rs[j].PurchasedAt) {
			return rs[i].Key() < rs[j].Key()
		}
		return rs[i].PurchasedAt.After(rs[j].PurchasedAt)
	})
}

// SortSummariesByDate orders summaries newest first.
func SortSummariesByDate(ss []Summary) {
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].PurchasedAt.Equal(ss[j].PurchasedAt) {
			return ss[i].Provider+ss[i].ID < ss[j].Provider+ss[j].ID
		}
		return ss[i].PurchasedAt.After(ss[j].PurchasedAt)
	})
}

// Dedup drops receipts whose key was already seen, keeping the first
// occurrence. Callers put the more trustworthy source first.
func Dedup(rs []Receipt) []Receipt {
	seen := make(map[string]bool, len(rs))
	out := rs[:0:0]
	for _, r := range rs {
		if seen[r.Key()] {
			continue
		}
		seen[r.Key()] = true
		out = append(out, r)
	}
	return out
}

// DropOverlapping removes receipts from extra that describe a purchase already
// present in primary. Two sources of the same store rarely agree on an id — an
// order confirmation mail may not carry the order number at all — so a same-day
// receipt for the same amount is treated as the same purchase.
func DropOverlapping(primary, extra []Receipt) []Receipt {
	if len(primary) == 0 || len(extra) == 0 {
		return extra
	}
	type fingerprint struct {
		day      string
		minor    int64
		currency string
	}
	seen := make(map[fingerprint]bool, len(primary))
	keys := make(map[string]bool, len(primary))
	for _, r := range primary {
		keys[r.Key()] = true
		seen[fingerprint{r.PurchasedAt.Format("2006-01-02"), r.Total.Minor, r.Total.Currency}] = true
	}

	out := extra[:0:0]
	for _, r := range extra {
		if keys[r.Key()] {
			continue
		}
		if seen[fingerprint{r.PurchasedAt.Format("2006-01-02"), r.Total.Minor, r.Total.Currency}] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// InRange reports whether the receipt falls inside [from, to]. Zero bounds are
// treated as open ends.
func (r Receipt) InRange(from, to time.Time) bool {
	if !from.IsZero() && r.PurchasedAt.Before(from) {
		return false
	}
	if !to.IsZero() && r.PurchasedAt.After(to) {
		return false
	}
	return true
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
