package lidl

// EXPERIMENTAL phone+password login for Lidl Plus.
//
// Lidl's web login is protected by reCAPTCHA, which is why the community
// projects (and this server's refresh_token path) rely on a one-time browser
// login. The mobile app instead uses native login/MFA endpoints that appear in
// the decompiled APK (account/login/mobile, account/mfa). This flow targets
// those: submit phone+password, then the SMS code, then complete the OAuth
// Authorization-Code+PKCE exchange for a refresh token.
//
// The exact request shapes of those private endpoints are not published, so the
// login and MFA URLs are overridable via the login_url / mfa_url fields; a
// failure surfaces the server's real response so the values can be corrected
// (capture them once with mitmproxy per docs/reverse-engineering.md). Whatever
// completes ends by storing a refresh token, exactly like the refresh_token
// path.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
)

const (
	// oauthScopes and the redirect are the app's, from the decompiled client.
	oauthScopes = "openid profile offline_access lpprofile lpapis"
	redirectURI = "com.lidlplus.app://callback"

	// Best-effort defaults for the app's native login endpoints; overridable
	// via the login_url / mfa_url fields when a capture shows the real ones.
	defaultLoginURL = authAPI + "/account/login/mobile"
	defaultMFAURL   = authAPI + "/account/mfa"

	pendingTTL = 10 * time.Minute
)

// pendingLogin is an interactive login waiting for its SMS code. It keeps the
// cookie-jar HTTP client so the IdentityServer session survives between calls.
type pendingLogin struct {
	client    *http.Client
	verifier  string
	loginURL  string
	mfaURL    string
	country   string
	language  string
	createdAt time.Time
}

// startInteractive begins a phone+password login and asks for the SMS code.
func (p *Provider) startInteractive(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	phone := strings.TrimSpace(fields["phone"])
	password := fields["password"]

	verifier, challenge, err := pkcePair()
	if err != nil {
		return provider.LoginResult{}, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return provider.LoginResult{}, err
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Stop at the custom-scheme callback so we can read its code.
			return http.ErrUseLastResponse
		},
	}

	// Establish the OAuth authorize session (best-effort; anti-forgery cookies).
	authorize := authAPI + "/connect/authorize?" + url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"scope":                 {oauthScopes},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	if req, e := http.NewRequestWithContext(ctx, http.MethodGet, authorize, nil); e == nil {
		if resp, e2 := client.Do(req); e2 == nil {
			resp.Body.Close()
		}
	}

	loginURL := firstNonEmpty(strings.TrimSpace(fields["login_url"]), defaultLoginURL)
	mfaURL := firstNonEmpty(strings.TrimSpace(fields["mfa_url"]), defaultMFAURL)

	// Submit the credentials. The mobile endpoint answers with an MFA challenge
	// (an SMS is sent) on success.
	body := map[string]string{"phoneNumber": phone, "password": password}
	if _, err := postJSON(ctx, client, loginURL, body, nil); err != nil {
		return provider.LoginResult{Provider: ID, OK: false, Message: fmt.Sprintf(
			"lidl: phone+password login could not start: %v; set login_url to the app's real login endpoint (capture it per docs/reverse-engineering.md), or paste a refresh_token", err)}, nil
	}

	country := strings.ToUpper(strings.TrimSpace(fields["country"]))
	if country == "" {
		country = p.defaultCountry
	}
	language := strings.ToLower(strings.TrimSpace(fields["language"]))
	if language == "" {
		language = p.defaultLanguage
	}

	token := ID + ":" + randomToken()
	p.putPending(token, &pendingLogin{
		client:    client,
		verifier:  verifier,
		loginURL:  loginURL,
		mfaURL:    mfaURL,
		country:   country,
		language:  language,
		createdAt: time.Now(),
	})

	return provider.LoginResult{
		Provider: ID,
		OK:       false,
		Message:  "Lidl Plus sent an SMS code",
		Next: &provider.LoginNext{
			Prompt:       "Enter the SMS code Lidl Plus just sent",
			Continuation: token,
			Fields: []provider.Field{
				{Name: "code", Description: "the SMS one-time code", Required: true, Secret: false},
			},
		},
	}, nil
}

// resumeInteractive submits the SMS code, finishes the OAuth exchange and stores
// the resulting refresh token.
func (p *Provider) resumeInteractive(ctx context.Context, continuation string, fields map[string]string) (provider.LoginResult, error) {
	pend := p.takePending(continuation)
	if pend == nil {
		return provider.LoginResult{}, errors.New("lidl: this login expired or was not found; start again with phone+password")
	}
	code := strings.TrimSpace(fields["code"])
	if code == "" {
		return provider.LoginResult{}, errors.New("lidl: the SMS code is required")
	}

	// Submit the SMS code; the endpoint completes the login and the authorize
	// redirect then carries the OAuth code to the app's callback.
	resp, err := postJSON(ctx, pend.client, pend.mfaURL, map[string]string{"code": code}, nil)
	if err != nil {
		return provider.LoginResult{Provider: ID, OK: false, Message: fmt.Sprintf(
			"lidl: the SMS code was not accepted: %v; set mfa_url to the app's real endpoint or paste a refresh_token", err)}, nil
	}

	authCode := codeFromLocation(resp)
	if authCode == "" {
		return provider.LoginResult{Provider: ID, OK: false, Message: "lidl: login did not yield an authorization code; the mobile login flow needs the real captured endpoints (see docs/reverse-engineering.md), or paste a refresh_token"}, nil
	}

	refresh, err := p.exchangeCode(ctx, pend.client, authCode, pend.verifier)
	if err != nil {
		return provider.LoginResult{}, err
	}
	return p.storeAndVerify(ctx, refresh, map[string]string{"country": pend.country, "language": pend.language})
}

// exchangeCode trades an authorization code for tokens and returns the refresh.
func (p *Provider) exchangeCode(ctx context.Context, client *http.Client, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"client_id":     {clientID},
	}
	basic := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authAPI+"/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Accept", "application/json")
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := doJSON(client, req, &resp); err != nil {
		return "", fmt.Errorf("%w: token exchange failed: %v", provider.ErrCredentialsRejected, err)
	}
	if resp.RefreshToken == "" {
		return "", fmt.Errorf("%w: token exchange returned no refresh_token", provider.ErrCredentialsRejected)
	}
	return resp.RefreshToken, nil
}

func (p *Provider) putPending(token string, pl *pendingLogin) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if p.pending == nil {
		p.pending = map[string]*pendingLogin{}
	}
	// Drop anything stale so an abandoned login cannot accumulate.
	for k, v := range p.pending {
		if time.Since(v.createdAt) > pendingTTL {
			delete(p.pending, k)
		}
	}
	p.pending[token] = pl
}

func (p *Provider) takePending(token string) *pendingLogin {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	pl := p.pending[token]
	if pl == nil {
		return nil
	}
	delete(p.pending, token)
	if time.Since(pl.createdAt) > pendingTTL {
		return nil
	}
	return pl
}

func pkcePair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomToken() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// postJSON sends a JSON body and returns the response (body already drained and
// closed; headers, status and any redirect Location remain readable). A 3xx is
// not an error — the client is configured to surface it so the caller can read
// the callback code; 4xx/5xx become an error carrying a short body snippet.
func postJSON(ctx context.Context, client *http.Client, urlStr string, body map[string]string, headers map[string]string) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("App-Version", appVersion)
	req.Header.Set("Operating-System", operatingSys)
	req.Header.Set("App", appPackage)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return resp, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

// doJSON runs a prepared request and decodes a JSON body into out.
func doJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		snippet := string(data)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(snippet))
	}
	return json.Unmarshal(data, out)
}

// codeFromLocation pulls ?code= out of a redirect to the app's callback scheme.
func codeFromLocation(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return ""
	}
	if u, err := url.Parse(loc); err == nil {
		if c := u.Query().Get("code"); c != "" {
			return c
		}
	}
	return ""
}
