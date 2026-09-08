package mailbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
)

// defaultLookback bounds an unbounded listing: without a from date, going back
// a year keeps one call from walking a decade of mail.
const defaultLookback = 365 * 24 * time.Hour

// searchMultiplier over-fetches messages relative to the requested receipt
// count, because most matched mails turn out not to be purchases.
const searchMultiplier = 4

// Source serves one store's receipts out of the mailbox. It satisfies
// provider.Provider so it can be used as a fallback behind the store's API.
type Source struct {
	client *Client
	rule   *Rule
	name   string
}

// NewSource pairs the mailbox client with the rule for one store. It returns
// nil when no rule is configured for that store, which leaves the store's API
// without a fallback rather than pretending to have one.
func NewSource(client *Client, rules Rules, providerID, displayName string) *Source {
	rule, ok := rules[providerID]
	if !ok {
		return nil
	}
	return &Source{client: client, rule: rule, name: displayName}
}

// ID implements provider.Provider.
func (s *Source) ID() string { return s.rule.Provider }

// DisplayName implements provider.Provider.
func (s *Source) DisplayName() string { return s.name + " (e-mail)" }

// Status implements provider.Provider.
func (s *Source) Status(ctx context.Context) provider.Status {
	return provider.Status{
		Provider:     s.rule.Provider,
		DisplayName:  s.DisplayName(),
		LoggedIn:     s.client.Configured(),
		StoredFields: s.client.Fields(),
		Sources:      []string{"email"},
		Notes: []string{
			fmt.Sprintf("matches mail from %v", s.rule.Senders),
		},
	}
}

// Login implements provider.Provider. Mailbox credentials are shared by every
// store, so they are set through the "mail" provider, not through this one.
func (s *Source) Login(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	return provider.LoginResult{}, fmt.Errorf("%w: configure the mailbox once with receipts_login provider=\"mail\"", provider.ErrNotSupported)
}

// Logout implements provider.Provider; see Login.
func (s *Source) Logout(ctx context.Context) error {
	return fmt.Errorf("%w: use receipts_logout provider=\"mail\"", provider.ErrNotSupported)
}

// List implements provider.Provider.
func (s *Source) List(ctx context.Context, q provider.Query) ([]receipt.Receipt, error) {
	q = q.Normalize()
	from := q.From
	if from.IsZero() {
		from = time.Now().Add(-defaultLookback)
	}

	messages, err := s.client.Search(ctx, s.rule.Senders, from, q.To, q.Limit*searchMultiplier)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil, provider.ErrNotLoggedIn
		}
		return nil, err
	}

	out := make([]receipt.Receipt, 0, len(messages))
	for _, msg := range messages {
		if !s.rule.Matches(msg) {
			continue
		}
		r, ok := s.rule.Parse(msg)
		if !ok || !r.InRange(q.From, q.To) {
			continue
		}
		out = append(out, r)
	}
	out = receipt.Dedup(out)
	receipt.SortByDate(out)
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Get implements provider.Provider by scanning the recent window for the id.
func (s *Source) Get(ctx context.Context, id string) (receipt.Receipt, error) {
	receipts, err := s.List(ctx, provider.Query{Limit: 500})
	if err != nil {
		return receipt.Receipt{}, err
	}
	for _, r := range receipts {
		if r.ID == id {
			return r, nil
		}
	}
	return receipt.Receipt{}, fmt.Errorf("mailbox: no %s receipt with id %q in the recent mail", s.rule.Provider, id)
}

// Probe reports what the mailbox search finds for this store, which is how a
// rule gets tuned without guessing: it says how many mails the senders matched,
// how many passed the subject filter and how many parsed into a receipt.
type Probe struct {
	Provider   string   `json:"provider"`
	Senders    []string `json:"senders"`
	Matched    int      `json:"messages_from_senders"`
	Accepted   int      `json:"messages_after_subject_filter"`
	Parsed     int      `json:"receipts_parsed"`
	Subjects   []string `json:"sample_subjects,omitempty"`
	Unparsed   []string `json:"sample_unparsed_subjects,omitempty"`
	SearchFrom string   `json:"search_from"`
}

// Probe runs the search and reports what happened, without returning receipts.
func (s *Source) Probe(ctx context.Context, days, sampleSize int) (Probe, error) {
	if days <= 0 {
		days = 90
	}
	if sampleSize <= 0 {
		sampleSize = 5
	}
	from := time.Now().AddDate(0, 0, -days)

	report := Probe{
		Provider:   s.rule.Provider,
		Senders:    s.rule.Senders,
		SearchFrom: from.Format("2006-01-02"),
	}
	messages, err := s.client.Search(ctx, s.rule.Senders, from, time.Time{}, 200)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return report, provider.ErrNotLoggedIn
		}
		return report, err
	}
	report.Matched = len(messages)

	for _, msg := range messages {
		if len(report.Subjects) < sampleSize {
			report.Subjects = append(report.Subjects, msg.Subject)
		}
		if !s.rule.Matches(msg) {
			continue
		}
		report.Accepted++
		if _, ok := s.rule.Parse(msg); ok {
			report.Parsed++
		} else if len(report.Unparsed) < sampleSize {
			report.Unparsed = append(report.Unparsed, msg.Subject)
		}
	}
	return report, nil
}
