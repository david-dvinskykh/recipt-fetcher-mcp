package action

// Website (www.action.com) login flow: a plain email+password POST that sets a
// session cookie, after which receipts come from the site's own GraphQL API
// (Apollo persisted queries), not the app gateway. This is what a browser does,
// and it works with the account's own password — recovered from a captured HAR
// (see docs/reverse-engineering.md). The response shapes match the app's, so
// the same receipt mapping and paging loop are reused; the only web-specific
// bits are the transport headers below.
//
// The GraphQL endpoint sits behind two guards that a naive client trips:
//   - Cloudflare's managed challenge, which a plain Go User-Agent fails ("Just
//     a moment…"); a browser-like User-Agent plus the __cf_bm cookie the login
//     response sets is enough to pass it.
//   - Apollo Server's CSRF prevention, which rejects a bare GET unless it
//     carries x-apollo-operation-name or apollo-require-preflight.
//
// Both were confirmed empirically against the live site.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/jsonx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
)

const (
	webLoginURL   = "https://www.action.com/api/auth/login"
	webGraphQLURL = "https://www.action.com/api/graphql"

	// A browser-like User-Agent is required to clear Cloudflare's managed
	// challenge on the GraphQL path; a default Go agent is served the JS
	// challenge page instead.
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	webReferer   = "https://www.action.com/"

	// Apollo persisted-query hashes captured from the site (operationName +
	// sha256Hash uniquely address a server-whitelisted query).
	webReceiptListHash   = "adff608bd7a8f11ff6b4ca54a480670ebbaa42f0139f28a3562787cd20bf9025"
	webReceiptDetailHash = "9bfa4d9ec37b5d8e1e6efcfe62a75bec521d423351cfe73a8f7cec732ca6fdc8"
)

func (p *Provider) ensureWebClient() {
	p.webOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		p.webClient = &http.Client{Timeout: 30 * time.Second, Jar: jar}
	})
}

// webLogin signs in to the website with the stored email+password, so the
// cookie jar holds a session (and the __cf_bm token) for the GraphQL calls.
func (p *Provider) webLogin(ctx context.Context) error {
	p.ensureWebClient()
	email := p.store.Field(ID, "email")
	password := p.store.Field(ID, "password")
	if email == "" || password == "" {
		return provider.ErrNotLoggedIn
	}
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webLoginURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", webReferer)
	resp, err := p.webClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("%w: the website rejected the e-mail/password (HTTP %d)", provider.ErrCredentialsRejected, resp.StatusCode)
	default:
		return fmt.Errorf("action: website login returned HTTP %d", resp.StatusCode)
	}
}

// webGraphQL runs one persisted GraphQL operation, logging in once if the
// session is missing or expired.
func (p *Provider) webGraphQL(ctx context.Context, gatewayOp string, vars map[string]any) (jsonx.Object, error) {
	op, hash, err := webOp(gatewayOp)
	if err != nil {
		return nil, err
	}
	p.ensureWebClient()

	data, needLogin, err := p.webCall(ctx, op, hash, vars)
	if err != nil {
		return nil, err
	}
	if needLogin {
		if err := p.webLogin(ctx); err != nil {
			return nil, err
		}
		data, needLogin, err = p.webCall(ctx, op, hash, vars)
		if err != nil {
			return nil, err
		}
		if needLogin {
			return nil, fmt.Errorf("%w: the website session could not be established", provider.ErrCredentialsRejected)
		}
	}
	return data, nil
}

// webCall performs the GET and returns the data object; needLogin is true when
// the response looks like an expired/absent session (so the caller re-logs in).
func (p *Provider) webCall(ctx context.Context, op, hash string, vars map[string]any) (data jsonx.Object, needLogin bool, err error) {
	varsJSON, _ := json.Marshal(vars)
	extJSON, _ := json.Marshal(map[string]any{
		"persistedQuery": map[string]any{"sha256Hash": hash, "version": 1},
	})
	q := url.Values{
		"operationName": {op},
		"variables":     {string(varsJSON)},
		"extensions":    {string(extJSON)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webGraphQLURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", webReferer)
	req.Header.Set("apollographql-client-name", "web")
	// Apollo CSRF prevention: a bare GET is blocked unless one of these is set.
	req.Header.Set("x-apollo-operation-name", op)
	req.Header.Set("apollo-require-preflight", "true")

	resp, err := p.webClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, true, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("%w: website GraphQL HTTP %d", provider.ErrEndpointUnavailable, resp.StatusCode)
	}
	body, err := jsonx.Decode(raw)
	if err != nil {
		return nil, false, fmt.Errorf("%w: website response was not JSON: %v", provider.ErrEndpointUnavailable, err)
	}
	if errs, _ := body.Array("errors"); len(errs) > 0 {
		text := strings.ToLower(errorText(errs))
		if strings.Contains(text, "auth") || strings.Contains(text, "unauth") ||
			strings.Contains(text, "forbidden") || strings.Contains(text, "session") ||
			strings.Contains(text, "login") {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("action: website GraphQL errors: %s", truncate(errorText(errs)))
	}
	data = body.Object("data")
	if data == nil {
		// A null data with no explicit error is most often a dropped session.
		return nil, true, nil
	}
	return data, false, nil
}

// webOp maps a gateway operation name onto the website's operation + persisted
// hash. The response shapes are identical, so callers reuse the app mapping.
func webOp(gatewayOp string) (op, hash string, err error) {
	switch gatewayOp {
	case "GetReceipts":
		return "ReceiptList", webReceiptListHash, nil
	case "GetSingleReceipt":
		return "ReceiptDetails", webReceiptDetailHash, nil
	default:
		return "", "", fmt.Errorf("action: no website operation for %q", gatewayOp)
	}
}

// stripCurrencyWords keeps only the numeric part of a formatted amount such as
// "8,99 zł" so money.Parse can read it.
func stripCurrencyWords(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == ',' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
