package mailbox

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

//go:embed rules.json
var defaultRulesJSON []byte

// Rule says how to recognize one store's mails and how to read the numbers out
// of them. Everything here is data on purpose: when a shop changes its mail
// template, fixing the extraction is a JSON edit, not a rebuild.
//
// All patterns are Go regular expressions and are matched case-insensitively
// against the flattened plain text of the mail. The first capture group is the
// value; item patterns use the named groups name, quantity and price.
type Rule struct {
	// Provider is the store id these mails belong to, e.g. "allegro".
	Provider string `json:"provider"`
	// StoreName is what to show as the shop of a mail-derived receipt.
	StoreName string `json:"store_name"`
	// Senders are substrings matched against the From address.
	Senders []string `json:"senders"`
	// SubjectAny, when set, requires the subject to contain one of these.
	SubjectAny []string `json:"subject_any"`
	// SubjectNone skips mails whose subject contains one of these, which is how
	// shipping notices and returns are kept out of the spending list.
	SubjectNone []string `json:"subject_none"`
	// Currency is assumed when the amount in the mail carries no symbol.
	Currency string `json:"currency"`
	// TotalPatterns are tried in order; the first match wins.
	TotalPatterns []string `json:"total_patterns"`
	// NumberPatterns extract the order number, which is also the receipt id
	// and therefore what lets a mail-derived receipt deduplicate against the
	// same purchase read from the store's API.
	NumberPatterns []string `json:"number_patterns"`
	// DatePatterns extract a purchase date from the body; without a match the
	// mail's own date is used.
	DatePatterns []string `json:"date_patterns"`
	// ItemPatterns extract item lines. Most confirmation mails have none.
	ItemPatterns []string `json:"item_patterns"`
	// Link is where a human can look the purchase up.
	Link string `json:"link"`

	compiled *compiledRule
}

type compiledRule struct {
	total  []*regexp.Regexp
	number []*regexp.Regexp
	date   []*regexp.Regexp
	items  []*regexp.Regexp
}

// Rules is a set of rules keyed by provider id.
type Rules map[string]*Rule

// LoadRules returns the built-in rules, overridden per provider by the JSON
// file at path when one is given.
func LoadRules(path string) (Rules, error) {
	rules, err := parseRules(defaultRulesJSON)
	if err != nil {
		return nil, fmt.Errorf("mailbox: built-in rules are invalid: %w", err)
	}
	if path == "" {
		return rules, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mailbox: read rules %s: %w", path, err)
	}
	overrides, err := parseRules(data)
	if err != nil {
		return nil, fmt.Errorf("mailbox: rules %s: %w", path, err)
	}
	for id, rule := range overrides {
		rules[id] = rule
	}
	return rules, nil
}

func parseRules(data []byte) (Rules, error) {
	var list []*Rule
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	out := make(Rules, len(list))
	for _, rule := range list {
		if rule.Provider == "" {
			return nil, fmt.Errorf("a rule has no provider")
		}
		if err := rule.compile(); err != nil {
			return nil, fmt.Errorf("provider %s: %w", rule.Provider, err)
		}
		out[rule.Provider] = rule
	}
	return out, nil
}

func (r *Rule) compile() error {
	compiled := &compiledRule{}
	groups := []struct {
		name     string
		patterns []string
		target   *[]*regexp.Regexp
	}{
		{"total_patterns", r.TotalPatterns, &compiled.total},
		{"number_patterns", r.NumberPatterns, &compiled.number},
		{"date_patterns", r.DatePatterns, &compiled.date},
		{"item_patterns", r.ItemPatterns, &compiled.items},
	}
	for _, group := range groups {
		for _, pattern := range group.patterns {
			expression, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				return fmt.Errorf("%s: %q: %w", group.name, pattern, err)
			}
			*group.target = append(*group.target, expression)
		}
	}
	r.compiled = compiled
	return nil
}

// Matches reports whether a message belongs to this rule.
func (r *Rule) Matches(msg Message) bool {
	from := strings.ToLower(msg.From)
	senderOK := false
	for _, sender := range r.Senders {
		if strings.Contains(from, strings.ToLower(sender)) {
			senderOK = true
			break
		}
	}
	if !senderOK {
		return false
	}

	subject := strings.ToLower(msg.Subject)
	for _, blocked := range r.SubjectNone {
		if strings.Contains(subject, strings.ToLower(blocked)) {
			return false
		}
	}
	if len(r.SubjectAny) == 0 {
		return true
	}
	for _, wanted := range r.SubjectAny {
		if strings.Contains(subject, strings.ToLower(wanted)) {
			return true
		}
	}
	return false
}
