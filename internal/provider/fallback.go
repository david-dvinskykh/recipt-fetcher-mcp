package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
)

// Fallback serves a store from its private API when that works, and from a
// secondary source (the mailbox) when it does not. Both are asked on a listing
// so a gap in either source does not hide a purchase; the primary wins on
// duplicates because it carries the item lines.
type Fallback struct {
	Primary   Provider
	Secondary Provider
}

// NewFallback pairs a primary provider with a secondary one. A nil secondary
// gives back the primary unchanged.
func NewFallback(primary, secondary Provider) Provider {
	if secondary == nil {
		return primary
	}
	return &Fallback{Primary: primary, Secondary: secondary}
}

// ID returns the primary's id: the pair is one store to the caller.
func (f *Fallback) ID() string { return f.Primary.ID() }

// DisplayName returns the primary's display name.
func (f *Fallback) DisplayName() string { return f.Primary.DisplayName() }

// Status merges both sources into one report.
func (f *Fallback) Status(ctx context.Context) Status {
	primary := f.Primary.Status(ctx)
	secondary := f.Secondary.Status(ctx)

	status := primary
	status.Sources = []string{"api"}
	if secondary.LoggedIn {
		status.Sources = append(status.Sources, "email")
		status.LoggedIn = true
	}
	if !primary.LoggedIn && secondary.LoggedIn {
		status.Notes = append(status.Notes,
			"store API is not logged in; receipts come from the mailbox only, so item lines may be missing")
	}
	if !secondary.LoggedIn {
		status.Notes = append(status.Notes,
			"mailbox fallback is not configured; run receipts_login with provider \"mail\" to enable it")
	}
	return status
}

// Login routes credentials to the primary provider.
func (f *Fallback) Login(ctx context.Context, fields map[string]string) (LoginResult, error) {
	return f.Primary.Login(ctx, fields)
}

// Logout drops the primary's credentials. The shared mailbox credentials are
// left alone: they belong to the "mail" provider, not to this store.
func (f *Fallback) Logout(ctx context.Context) error { return f.Primary.Logout(ctx) }

// List asks both sources and merges them, primary first.
func (f *Fallback) List(ctx context.Context, q Query) ([]receipt.Receipt, error) {
	primary, primaryErr := f.Primary.List(ctx, q)
	if primaryErr != nil && !isFallbackWorthy(primaryErr) {
		return nil, primaryErr
	}

	secondary, secondaryErr := f.Secondary.List(ctx, q)
	if primaryErr != nil && secondaryErr != nil {
		return nil, fmt.Errorf("%s API: %v; mailbox fallback: %w", f.ID(), primaryErr, secondaryErr)
	}
	if primaryErr != nil {
		return annotate(secondary, fmt.Sprintf("%s API unavailable (%v); reconstructed from e-mail", f.ID(), primaryErr)), nil
	}
	if secondaryErr != nil && !isFallbackWorthy(secondaryErr) {
		return nil, secondaryErr
	}

	merged := receipt.Dedup(append(primary, receipt.DropOverlapping(primary, secondary)...))
	receipt.SortByDate(merged)
	if q.Limit > 0 && len(merged) > q.Limit {
		merged = merged[:q.Limit]
	}
	return merged, nil
}

// Get tries the primary, then the secondary.
func (f *Fallback) Get(ctx context.Context, id string) (receipt.Receipt, error) {
	r, err := f.Primary.Get(ctx, id)
	if err == nil {
		return r, nil
	}
	if !isFallbackWorthy(err) {
		return receipt.Receipt{}, err
	}
	secondary, secondErr := f.Secondary.Get(ctx, id)
	if secondErr != nil {
		return receipt.Receipt{}, fmt.Errorf("%s API: %v; mailbox fallback: %w", f.ID(), err, secondErr)
	}
	secondary.Notes = append(secondary.Notes,
		fmt.Sprintf("%s API unavailable (%v); reconstructed from e-mail", f.ID(), err))
	return secondary, nil
}

// isFallbackWorthy reports whether an error is the kind the mailbox can cover:
// missing or rejected credentials and endpoints that moved. A context
// cancellation or a caller mistake must surface instead of being papered over.
func isFallbackWorthy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, ErrNotLoggedIn) ||
		errors.Is(err, ErrCredentialsRejected) ||
		errors.Is(err, ErrEndpointUnavailable) ||
		errors.Is(err, ErrNotSupported)
}

func annotate(rs []receipt.Receipt, note string) []receipt.Receipt {
	for i := range rs {
		if !containsNote(rs[i].Notes, note) {
			rs[i].Notes = append(rs[i].Notes, note)
		}
	}
	return rs
}

func containsNote(notes []string, note string) bool {
	for _, n := range notes {
		if strings.EqualFold(n, note) {
			return true
		}
	}
	return false
}
