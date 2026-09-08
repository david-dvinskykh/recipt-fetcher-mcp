package mailbox

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/jsonx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
)

// Parse turns one matched message into a receipt. It returns false when the
// mail carries no total, because a purchase without an amount is of no use to
// whoever is categorizing spending.
func (r *Rule) Parse(msg Message) (receipt.Receipt, bool) {
	total, found := r.total(msg.Text)
	if !found {
		return receipt.Receipt{}, false
	}

	out := receipt.Receipt{
		Provider:      r.Provider,
		Source:        receipt.SourceEmail,
		PurchasedAt:   r.date(msg),
		Currency:      total.Currency,
		Total:         total,
		Store:         &receipt.Store{Name: r.StoreName},
		Link:          r.Link,
		ItemsComplete: false,
		Notes: []string{
			fmt.Sprintf("reconstructed from the e-mail %q of %s", strings.TrimSpace(msg.Subject), msg.Date.Format("2006-01-02")),
		},
	}

	number := r.match(r.compiled.number, msg.Text)
	out.Number = number
	out.ID = receiptID(r.Provider, number, msg)

	if number == "" {
		out.Notes = append(out.Notes,
			"no order number in the mail, so this receipt cannot be matched against the same purchase read from the store API")
	}

	out.Items = r.items(msg.Text, total.Currency)
	if len(out.Items) == 0 {
		out.Notes = append(out.Notes, "the mail carries no item lines; only the total is known")
	}
	return out, true
}

// total reads the receipt total.
func (r *Rule) total(text string) (money.Amount, bool) {
	raw := r.match(r.compiled.total, text)
	if raw == "" {
		return money.Amount{}, false
	}
	amount, err := money.Parse(raw, r.Currency)
	if err != nil {
		return money.Amount{}, false
	}
	if amount.Currency == "" {
		amount = money.New(amount.Minor, r.Currency)
	}
	return amount, true
}

// date prefers a date printed in the body, falling back to the mail's own.
func (r *Rule) date(msg Message) time.Time {
	if raw := r.match(r.compiled.date, msg.Text); raw != "" {
		if parsed := jsonx.ParseTimeString(normalizeDate(raw)); !parsed.IsZero() {
			return parsed
		}
	}
	return msg.Date
}

// items extracts item lines when the rule knows how to.
func (r *Rule) items(text, currency string) []receipt.Item {
	var out []receipt.Item
	for _, pattern := range r.compiled.items {
		for _, line := range strings.Split(text, "\n") {
			groups := namedGroups(pattern, strings.TrimSpace(line))
			if groups == nil {
				continue
			}
			price, err := money.Parse(groups["price"], currency)
			if err != nil {
				continue
			}
			quantity := 1.0
			if raw, ok := groups["quantity"]; ok && raw != "" {
				if parsed, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64); err == nil && parsed > 0 {
					quantity = parsed
				}
			}
			total := money.New(int64(float64(price.Minor)*quantity+0.5), price.Currency)
			out = append(out, receipt.Item{
				Name:      strings.TrimSpace(groups["name"]),
				Quantity:  quantity,
				UnitPrice: price,
				Gross:     total,
				Total:     total,
			})
		}
		if len(out) > 0 {
			break
		}
	}
	return out
}

// match returns the first capture group of the first pattern that matches.
func (r *Rule) match(patterns []*regexp.Regexp, text string) string {
	for _, pattern := range patterns {
		if groups := pattern.FindStringSubmatch(text); len(groups) > 1 {
			return strings.TrimSpace(groups[1])
		}
	}
	return ""
}

func namedGroups(pattern *regexp.Regexp, line string) map[string]string {
	matches := pattern.FindStringSubmatch(line)
	if matches == nil {
		return nil
	}
	out := make(map[string]string, len(matches))
	for i, name := range pattern.SubexpNames() {
		if name != "" && i < len(matches) {
			out[name] = matches[i]
		}
	}
	return out
}

// receiptID prefers the order number, so that a purchase seen both in the mail
// and in the store's API deduplicates to one receipt. Without a number it falls
// back to a hash of the message id, which is stable across runs.
func receiptID(provider, number string, msg Message) string {
	if number != "" {
		return number
	}
	sum := sha1.Sum([]byte(provider + "|" + msg.ID))
	return "mail-" + hex.EncodeToString(sum[:8])
}

// normalizeDate turns the dotted and slashed day-first dates used in these
// mails into a form the shared time parser accepts.
func normalizeDate(raw string) string {
	replaced := strings.NewReplacer("/", ".", "-", ".").Replace(strings.TrimSpace(raw))
	parts := strings.Split(replaced, ".")
	if len(parts) == 3 && len(parts[0]) == 2 && len(parts[2]) == 4 {
		return fmt.Sprintf("%s.%s.%s", parts[0], parts[1], parts[2])
	}
	return raw
}
