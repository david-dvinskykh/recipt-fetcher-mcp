// Package mcpserver exposes the receipt providers as MCP tools over stdio.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/config"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/httpx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/mailbox"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider/action"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider/allegro"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider/lidl"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/secret"
)

// Version is reported to MCP clients.
const Version = "1.0.0"

// connectMetaKey is the MetaMCP Connect Protocol (MCP-Connect v1) namespace.
// MetaMCP reads it from a tool's _meta to render a generic "Connect" button and
// form for this server; see docs/button-auth.md and metamcp docs/connect-protocol.md.
const connectMetaKey = "ai.metamcp.connect/v1"

// inlineExportLimit caps how much export data is returned in a tool result.
// Larger exports have to be written to a file.
const inlineExportLimit = 256 * 1024

// detailWorkers bounds how many receipt details are fetched at once, so a wide
// export does not hammer a store's API.
const detailWorkers = 4

// Server holds everything the tools operate on.
type Server struct {
	cfg       config.Config
	store     *secret.Store
	registry  *provider.Registry
	mail      *mailbox.Client
	mailRules mailbox.Rules
	sources   map[string]*mailbox.Source
}

// New wires the providers together.
func New(cfg config.Config) (*Server, error) {
	store, err := secret.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	rules, err := mailbox.LoadRules(cfg.MailRulesPath)
	if err != nil {
		return nil, err
	}

	client := httpx.New(cfg.UserAgent)
	mailClient := mailbox.NewClient(store)

	actionMail := mailbox.NewSource(mailClient, rules, action.ID, "Action")
	allegroMail := mailbox.NewSource(mailClient, rules, allegro.ID, "Allegro")

	srv := &Server{
		cfg:       cfg,
		store:     store,
		mail:      mailClient,
		mailRules: rules,
		sources:   map[string]*mailbox.Source{},
	}
	if actionMail != nil {
		srv.sources[action.ID] = actionMail
	}
	if allegroMail != nil {
		srv.sources[allegro.ID] = allegroMail
	}

	srv.registry = provider.NewRegistry(
		lidl.New(client, store, cfg.LidlCountry, cfg.LidlLanguage),
		provider.NewFallback(action.New(client, store), asProvider(actionMail)),
		provider.NewFallback(allegro.New(client, store), asProvider(allegroMail)),
	)
	return srv, nil
}

// asProvider avoids handing a typed nil to the fallback wrapper.
func asProvider(source *mailbox.Source) provider.Provider {
	if source == nil {
		return nil
	}
	return source
}

// MCPServer builds the MCP server with every tool registered.
func (s *Server) MCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "receipts",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "receipts_providers",
		Description: "List the supported stores, whether each one is logged in, which data sources it will use " +
			"and which credential fields receipts_login expects. Start here.",
	}, s.handleProviders)

	mcp.AddTool(server, &mcp.Tool{
		Name: "receipts_login",
		Description: "Store credentials for one store, or for the shared mailbox fallback (provider \"mail\"). " +
			"Credentials are encrypted on disk and are never returned by any tool. " +
			"Call receipts_providers first to see which fields the store needs.",
		// MCP-Connect v1: MetaMCP renders a "Connect stores" button and, per
		// store, a form built from receipts_providers.providers[].required_fields,
		// then calls this tool with {provider, fields}. See connectMetaKey.
		Meta: mcp.Meta{
			connectMetaKey: map[string]any{
				"kind":      "connect",
				"label":     "Connect stores",
				"group":     "receipts",
				"targetArg": "provider",
				"fieldsArg": "fields",
				"targets": map[string]any{
					"tool":      "receipts_providers",
					"path":      "providers",
					"id":        "provider",
					"label":     "display_name",
					"connected": "logged_in",
					"fields":    "required_fields",
					"notes":     "notes",
				},
			},
		},
	}, s.handleLogin)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "receipts_logout",
		Description: "Delete the stored credentials of one store, or of the mailbox (provider \"mail\").",
		// MCP-Connect v1: the disconnect action for a store; MetaMCP calls this
		// with {provider} and no form.
		Meta: mcp.Meta{
			connectMetaKey: map[string]any{
				"kind":      "disconnect",
				"label":     "Disconnect a store",
				"group":     "receipts",
				"targetArg": "provider",
			},
		},
	}, s.handleLogout)

	mcp.AddTool(server, &mcp.Tool{
		Name: "receipts_list",
		Description: "List receipt summaries across stores for a date range, newest first. " +
			"Summaries carry no item lines; use receipts_get for those.",
	}, s.handleList)

	mcp.AddTool(server, &mcp.Tool{
		Name: "receipts_get",
		Description: "Fetch one receipt with its item lines, taxes and discounts, ready to categorize. " +
			"Set include_raw to also get the untouched payload from the store.",
	}, s.handleGet)

	mcp.AddTool(server, &mcp.Tool{
		Name: "receipts_export",
		Description: "Export receipts for a date range as json, ndjson or csv. " +
			"With include_items the item lines of every receipt are fetched too (one CSV row per item). " +
			"Give a path to write a file instead of returning the data inline.",
	}, s.handleExport)

	mcp.AddTool(server, &mcp.Tool{
		Name: "receipts_mail_probe",
		Description: "Diagnose the e-mail fallback for one store: how many mails the configured senders match, " +
			"how many pass the subject filter and how many parse into a receipt. Use it to tune the parsing rules.",
	}, s.handleMailProbe)

	return server
}

const instructions = `This server fetches purchase receipts so they can be categorized.

Stores: lidl (Lidl Plus app API), action (Mijn Action), allegro (buyer purchases).
Action and Allegro fall back to parsing confirmation e-mail over IMAP when their
own endpoint is unavailable; a receipt says which source it came from and
whether its item lines are complete.

Typical flow: receipts_providers -> receipts_login (per store, once) ->
receipts_list for a period -> receipts_get for the receipts worth itemizing, or
receipts_export for the whole period at once.

Amounts are exact: every money value carries both a decimal string and integer
minor units. This server does not categorize anything itself.`

// --- receipts_providers ---

type providersInput struct{}

type providersOutput struct {
	Providers []provider.Status `json:"providers"`
	Mailbox   mailboxStatus     `json:"mailbox"`
	StateDir  string            `json:"state_dir"`
}

type mailboxStatus struct {
	Configured   bool             `json:"configured"`
	StoredFields []string         `json:"stored_fields,omitempty"`
	Stores       []string         `json:"stores_with_rules,omitempty"`
	Fields       []provider.Field `json:"required_fields"`
}

func (s *Server) handleProviders(ctx context.Context, req *mcp.CallToolRequest, in providersInput) (*mcp.CallToolResult, providersOutput, error) {
	out := providersOutput{StateDir: s.cfg.StateDir}
	for _, p := range s.registry.All() {
		out.Providers = append(out.Providers, p.Status(ctx))
	}

	stores := make([]string, 0, len(s.sources))
	for id := range s.sources {
		stores = append(stores, id)
	}
	sort.Strings(stores)

	out.Mailbox = mailboxStatus{
		Configured:   s.mail.Configured(),
		StoredFields: s.mail.Fields(),
		Stores:       stores,
		Fields: []provider.Field{
			{Name: "host", Description: "IMAP host, e.g. imap.gmail.com", Required: true},
			{Name: "port", Description: "IMAP TLS port, default 993", Required: false},
			{Name: "username", Description: "mailbox user, usually the e-mail address", Required: true},
			{Name: "password", Description: "mailbox password; for Gmail an app password, not the account password", Required: true, Secret: true},
			{Name: "mailbox", Description: "folder to search, default INBOX", Required: false},
		},
	}
	return nil, out, nil
}

// StatusReport is the same data receipts_providers returns, for the -status
// flag: checking credentials from a shell should not need an MCP client.
func (s *Server) StatusReport(ctx context.Context) (any, error) {
	_, out, err := s.handleProviders(ctx, nil, providersInput{})
	return out, err
}

// WebLogin performs a login exactly as the receipts_login tool does, so the
// HTTP auth web app (button-auth) can reuse the same validation and storage.
func (s *Server) WebLogin(ctx context.Context, providerID string, fields map[string]string) (provider.LoginResult, error) {
	id := strings.ToLower(strings.TrimSpace(providerID))
	if id == "" {
		return provider.LoginResult{}, errors.New("provider is required")
	}
	if len(fields) == 0 {
		return provider.LoginResult{}, errors.New("no credentials provided")
	}
	if id == mailbox.SecretID {
		if err := s.mail.Login(ctx, fields); err != nil {
			return provider.LoginResult{}, err
		}
		return provider.LoginResult{
			Provider: mailbox.SecretID,
			OK:       true,
			Message:  "mailbox credentials verified",
			Stored:   s.mail.Fields(),
		}, nil
	}
	p, err := s.registry.Get(id)
	if err != nil {
		return provider.LoginResult{}, err
	}
	return p.Login(ctx, fields)
}

// --- receipts_login ---

type loginInput struct {
	Provider string            `json:"provider" jsonschema:"store id (lidl, action, allegro) or \"mail\" for the shared IMAP fallback"`
	Fields   map[string]string `json:"fields" jsonschema:"credential fields for that provider, as listed by receipts_providers"`
}

type loginOutput struct {
	provider.LoginResult
}

func (s *Server) handleLogin(ctx context.Context, req *mcp.CallToolRequest, in loginInput) (*mcp.CallToolResult, loginOutput, error) {
	id := strings.ToLower(strings.TrimSpace(in.Provider))
	if id == "" {
		return nil, loginOutput{}, errors.New("provider is required")
	}
	if len(in.Fields) == 0 {
		return nil, loginOutput{}, errors.New("fields is required; call receipts_providers to see which fields this provider needs")
	}

	if id == mailbox.SecretID {
		if err := s.mail.Login(ctx, in.Fields); err != nil {
			return nil, loginOutput{}, err
		}
		return nil, loginOutput{provider.LoginResult{
			Provider: mailbox.SecretID,
			OK:       true,
			Message:  "mailbox credentials verified; Action and Allegro can now fall back to e-mail",
			Stored:   s.mail.Fields(),
		}}, nil
	}

	p, err := s.registry.Get(id)
	if err != nil {
		return nil, loginOutput{}, err
	}
	result, err := p.Login(ctx, in.Fields)
	if err != nil {
		return nil, loginOutput{}, err
	}
	return nil, loginOutput{result}, nil
}

// --- receipts_logout ---

type logoutInput struct {
	Provider string `json:"provider" jsonschema:"store id, or \"mail\" for the IMAP fallback"`
}

type logoutOutput struct {
	Provider string `json:"provider"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

func (s *Server) handleLogout(ctx context.Context, req *mcp.CallToolRequest, in logoutInput) (*mcp.CallToolResult, logoutOutput, error) {
	id := strings.ToLower(strings.TrimSpace(in.Provider))
	if id == mailbox.SecretID {
		if err := s.mail.Logout(); err != nil {
			return nil, logoutOutput{}, err
		}
		return nil, logoutOutput{Provider: id, OK: true, Message: "mailbox credentials deleted"}, nil
	}

	p, err := s.registry.Get(id)
	if err != nil {
		return nil, logoutOutput{}, err
	}
	if err := p.Logout(ctx); err != nil {
		return nil, logoutOutput{}, err
	}
	return nil, logoutOutput{Provider: id, OK: true, Message: "credentials deleted"}, nil
}

// --- receipts_list ---

type listInput struct {
	Providers []string `json:"providers,omitempty" jsonschema:"stores to query; empty means all of them"`
	From      string   `json:"from,omitempty" jsonschema:"start of the period, YYYY-MM-DD or RFC3339; empty means no lower bound"`
	To        string   `json:"to,omitempty" jsonschema:"end of the period, YYYY-MM-DD or RFC3339; empty means now"`
	Limit     int      `json:"limit,omitempty" jsonschema:"maximum receipts per store, default 50"`
}

type listOutput struct {
	Receipts []receipt.Summary `json:"receipts"`
	Errors   []providerError   `json:"errors,omitempty"`
	Count    int               `json:"count"`
}

type providerError struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
	Hint     string `json:"hint,omitempty"`
}

func (s *Server) handleList(ctx context.Context, req *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	q, err := buildQuery(in.From, in.To, in.Limit)
	if err != nil {
		return nil, listOutput{}, err
	}
	providers, err := s.registry.Resolve(normalizeIDs(in.Providers))
	if err != nil {
		return nil, listOutput{}, err
	}

	receipts, failures := s.collect(ctx, providers, q)
	out := listOutput{Errors: failures}
	for _, r := range receipts {
		out.Receipts = append(out.Receipts, r.Summarize())
	}
	receipt.SortSummariesByDate(out.Receipts)
	out.Count = len(out.Receipts)

	if out.Count == 0 && len(failures) == len(providers) && len(failures) > 0 {
		return nil, out, fmt.Errorf("no store could be queried: %s", joinErrors(failures))
	}
	return nil, out, nil
}

// --- receipts_get ---

type getInput struct {
	Provider   string `json:"provider" jsonschema:"store id the receipt belongs to"`
	ID         string `json:"id" jsonschema:"receipt id as returned by receipts_list"`
	IncludeRaw bool   `json:"include_raw,omitempty" jsonschema:"also return the untouched payload from the store"`
}

type getOutput struct {
	Receipt receipt.Receipt `json:"receipt"`
}

func (s *Server) handleGet(ctx context.Context, req *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	if strings.TrimSpace(in.ID) == "" {
		return nil, getOutput{}, errors.New("id is required")
	}
	p, err := s.registry.Get(strings.ToLower(strings.TrimSpace(in.Provider)))
	if err != nil {
		return nil, getOutput{}, err
	}
	r, err := p.Get(ctx, in.ID)
	if err != nil {
		return nil, getOutput{}, decorate(p.ID(), err)
	}
	if !in.IncludeRaw {
		r.Raw = nil
	}
	return nil, getOutput{Receipt: r}, nil
}

// --- receipts_export ---

type exportInput struct {
	Providers    []string `json:"providers,omitempty" jsonschema:"stores to export; empty means all of them"`
	From         string   `json:"from,omitempty" jsonschema:"start of the period, YYYY-MM-DD or RFC3339"`
	To           string   `json:"to,omitempty" jsonschema:"end of the period, YYYY-MM-DD or RFC3339"`
	Limit        int      `json:"limit,omitempty" jsonschema:"maximum receipts per store, default 50"`
	Format       string   `json:"format,omitempty" jsonschema:"json (default), ndjson or csv"`
	IncludeItems bool     `json:"include_items,omitempty" jsonschema:"fetch the item lines of every receipt; slower, and required for a useful csv"`
	Path         string   `json:"path,omitempty" jsonschema:"write the export to this file instead of returning it inline"`
}

type exportOutput struct {
	Format    string          `json:"format"`
	Count     int             `json:"receipt_count"`
	Bytes     int             `json:"bytes"`
	Path      string          `json:"path,omitempty"`
	Data      string          `json:"data,omitempty"`
	Errors    []providerError `json:"errors,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Note      string          `json:"note,omitempty"`
}

func (s *Server) handleExport(ctx context.Context, req *mcp.CallToolRequest, in exportInput) (*mcp.CallToolResult, exportOutput, error) {
	format, err := receipt.ParseFormat(in.Format)
	if err != nil {
		return nil, exportOutput{}, err
	}
	q, err := buildQuery(in.From, in.To, in.Limit)
	if err != nil {
		return nil, exportOutput{}, err
	}
	providers, err := s.registry.Resolve(normalizeIDs(in.Providers))
	if err != nil {
		return nil, exportOutput{}, err
	}

	receipts, failures := s.collect(ctx, providers, q)
	if in.IncludeItems {
		var itemErrors []providerError
		receipts, itemErrors = s.withItems(ctx, receipts)
		failures = append(failures, itemErrors...)
	}
	receipt.SortByDate(receipts)

	data, err := receipt.Encode(receipts, format)
	if err != nil {
		return nil, exportOutput{}, err
	}

	out := exportOutput{
		Format: string(format),
		Count:  len(receipts),
		Bytes:  len(data),
		Errors: failures,
	}
	if !in.IncludeItems && format == receipt.FormatCSV {
		out.Note = "exported without item lines; pass include_items=true for one row per item"
	}

	if path := strings.TrimSpace(in.Path); path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, exportOutput{}, err
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			return nil, exportOutput{}, err
		}
		if err := os.WriteFile(absolute, data, 0o600); err != nil {
			return nil, exportOutput{}, err
		}
		out.Path = absolute
		return nil, out, nil
	}

	if len(data) > inlineExportLimit {
		out.Truncated = true
		out.Data = string(data[:inlineExportLimit])
		out.Note = fmt.Sprintf("export is %d bytes, cut at %d; pass a path to write the whole file", len(data), inlineExportLimit)
		return nil, out, nil
	}
	out.Data = string(data)
	return nil, out, nil
}

// --- receipts_mail_probe ---

type mailProbeInput struct {
	Provider string `json:"provider" jsonschema:"store whose e-mail rule to test, e.g. allegro or action"`
	Days     int    `json:"days,omitempty" jsonschema:"how far back to search, default 90"`
}

type mailProbeOutput struct {
	Probe mailbox.Probe `json:"probe"`
	Hint  string        `json:"hint,omitempty"`
}

func (s *Server) handleMailProbe(ctx context.Context, req *mcp.CallToolRequest, in mailProbeInput) (*mcp.CallToolResult, mailProbeOutput, error) {
	id := strings.ToLower(strings.TrimSpace(in.Provider))
	source, ok := s.sources[id]
	if !ok {
		return nil, mailProbeOutput{}, fmt.Errorf("no e-mail rule for provider %q; rules exist for %v", id, s.ruleProviders())
	}
	probe, err := source.Probe(ctx, in.Days, 5)
	if err != nil {
		return nil, mailProbeOutput{}, decorate(id, err)
	}

	out := mailProbeOutput{Probe: probe}
	switch {
	case probe.Matched == 0:
		out.Hint = "no mail from those senders: check the sender list in the rules file (RECEIPTS_MAIL_RULES) and the mailbox folder"
	case probe.Accepted == 0:
		out.Hint = "mail found but the subject filter rejected all of it: relax subject_any or subject_none in the rules"
	case probe.Parsed == 0:
		out.Hint = "subjects match but no total could be read: adjust total_patterns in the rules"
	}
	return nil, out, nil
}

// collect queries every provider concurrently and keeps going when one fails.
func (s *Server) collect(ctx context.Context, providers []provider.Provider, q provider.Query) ([]receipt.Receipt, []providerError) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		receipts []receipt.Receipt
		failures []providerError
	)
	for _, p := range providers {
		wg.Add(1)
		go func(p provider.Provider) {
			defer wg.Done()
			found, err := p.List(ctx, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, toProviderError(p.ID(), err))
				return
			}
			receipts = append(receipts, found...)
		}(p)
	}
	wg.Wait()

	receipt.SortByDate(receipts)
	sort.Slice(failures, func(i, j int) bool { return failures[i].Provider < failures[j].Provider })
	return receipts, failures
}

// withItems fills in the item lines of receipts that have none.
func (s *Server) withItems(ctx context.Context, receipts []receipt.Receipt) ([]receipt.Receipt, []providerError) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		failures []providerError
	)
	semaphore := make(chan struct{}, detailWorkers)

	for i := range receipts {
		if receipts[i].ItemsComplete {
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			p, err := s.registry.Get(receipts[index].Provider)
			if err != nil {
				return
			}
			detailed, err := p.Get(ctx, receipts[index].ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, providerError{
					Provider: receipts[index].Provider,
					Error:    fmt.Sprintf("receipt %s: %v", receipts[index].ID, err),
				})
				return
			}
			detailed.Raw = nil
			detailed.Notes = append(detailed.Notes, receipts[index].Notes...)
			receipts[index] = detailed
		}(i)
	}
	wg.Wait()
	return receipts, failures
}

func (s *Server) ruleProviders() []string {
	out := make([]string, 0, len(s.sources))
	for id := range s.sources {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// buildQuery parses the date arguments.
func buildQuery(from, to string, limit int) (provider.Query, error) {
	q := provider.Query{Limit: limit}
	if strings.TrimSpace(from) != "" {
		parsed, err := parseDate(from, false)
		if err != nil {
			return q, fmt.Errorf("from: %w", err)
		}
		q.From = parsed
	}
	if strings.TrimSpace(to) != "" {
		parsed, err := parseDate(to, true)
		if err != nil {
			return q, fmt.Errorf("to: %w", err)
		}
		q.To = parsed
	}
	if !q.From.IsZero() && !q.To.IsZero() && q.To.Before(q.From) {
		return q, errors.New("to is before from")
	}
	return q.Normalize(), nil
}

// parseDate accepts a plain date or a full timestamp. A plain date used as an
// upper bound means the end of that day, so "to: 2026-09-07" includes the 7th.
func parseDate(value string, endOfDay bool) (time.Time, error) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither YYYY-MM-DD nor RFC3339", value)
	}
	if endOfDay {
		return parsed.Add(24*time.Hour - time.Nanosecond), nil
	}
	return parsed, nil
}

func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.ToLower(strings.TrimSpace(id))
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// toProviderError turns a provider failure into an actionable line.
func toProviderError(id string, err error) providerError {
	out := providerError{Provider: id, Error: err.Error()}
	switch {
	case errors.Is(err, provider.ErrNotLoggedIn):
		out.Hint = fmt.Sprintf("call receipts_login for %q; receipts_providers lists the fields it needs", id)
	case errors.Is(err, provider.ErrCredentialsRejected):
		out.Hint = fmt.Sprintf("the stored credentials for %q no longer work; log in again", id)
	case errors.Is(err, provider.ErrEndpointUnavailable):
		out.Hint = "the store's endpoint changed; configure the mailbox fallback with receipts_login provider=\"mail\""
	case errors.Is(err, provider.ErrNotSupported):
		out.Hint = "this store has no working data source configured yet"
	}
	return out
}

func decorate(id string, err error) error {
	failure := toProviderError(id, err)
	if failure.Hint == "" {
		return err
	}
	return fmt.Errorf("%w (%s)", err, failure.Hint)
}

func joinErrors(failures []providerError) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.Provider+": "+failure.Error)
	}
	return strings.Join(parts, "; ")
}
