package lidl

// Browser login for Lidl Plus, driven from MetaMCP's Connect panel.
//
// Lidl's login page is guarded by reCAPTCHA Enterprise + Akamai and finishes
// with an SMS/e-mail 2FA code, so it cannot run headless on the server. Instead
// this flow runs in the *user's own browser* (where the captcha scores well and
// the SMS arrives): startConnect returns the app's OAuth authorize URL for
// MetaMCP to open in a new tab; the user logs in and the browser is redirected
// to com.lidlplus.app://callback?code=... (which the browser can't open, so it
// shows the URL); the user pastes that code back and resumeConnect exchanges it
// for a refresh token — exactly the Authorization-Code + PKCE dance the app
// does. The tools/lidl-login helper automates the same thing locally.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
)

const (
	// The app's OAuth scopes and native redirect, from the decompiled client.
	oauthScopes = "openid profile offline_access lpprofile lpapis"
	redirectURI = "com.lidlplus.app://callback"

	pendingTTL = 15 * time.Minute
)

// pendingConnect is a browser login waiting for its OAuth code. It holds the
// PKCE verifier and the resolved country/language between the two calls.
type pendingConnect struct {
	verifier  string
	country   string
	language  string
	createdAt time.Time
}

// startConnect begins the browser login and returns the authorize URL to open
// plus a prompt for the code the user will paste back.
func (p *Provider) startConnect(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return provider.LoginResult{}, err
	}

	country := strings.ToUpper(strings.TrimSpace(fields["country"]))
	if country == "" {
		country = p.defaultCountry
	}
	if country == "" {
		country = "PL"
	}
	language := strings.ToLower(strings.TrimSpace(fields["language"]))
	if language == "" {
		language = p.defaultLanguage
	}
	if language == "" {
		language = strings.ToLower(country)
	}

	authorize := authAPI + "/connect/authorize?" + url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"scope":                 {oauthScopes},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"Country":               {country},
		"language":              {language + "-" + country},
	}.Encode()

	token := ID + ":" + randomToken()
	p.putPending(token, &pendingConnect{
		verifier:  verifier,
		country:   country,
		language:  language,
		createdAt: time.Now(),
	})

	return provider.LoginResult{
		Provider: ID,
		OK:       false,
		Message:  "Log in to Lidl Plus in the tab that opens",
		Next: &provider.LoginNext{
			Prompt:  "A Lidl login tab opened — sign in there (solve the captcha and enter the SMS code). When done, your browser tries to open a link starting with com.lidlplus.app://callback?code=… and shows an error — that is expected. Paste that whole address (or just the code= value) here.",
			OpenURL: authorize,
			Fields: []provider.Field{
				{Name: "code", Description: "the code from the com.lidlplus.app://callback address after you log in (the whole URL is fine)", Required: true, Secret: false},
			},
			Continuation: token,
		},
	}, nil
}

// resumeConnect exchanges the pasted OAuth code for a refresh token and stores
// it, verifying it once.
func (p *Provider) resumeConnect(ctx context.Context, continuation string, fields map[string]string) (provider.LoginResult, error) {
	pend := p.takePending(continuation)
	if pend == nil {
		return provider.LoginResult{}, errors.New("lidl: this login expired or was not found; start again from Connect")
	}
	code := parseCode(fields["code"])
	if code == "" {
		return provider.LoginResult{}, errors.New("lidl: no code found — paste the com.lidlplus.app://callback address (or its code= value) from your browser")
	}

	refresh, err := p.exchangeCode(ctx, code, pend.verifier)
	if err != nil {
		return provider.LoginResult{}, err
	}
	return p.storeAndVerify(ctx, refresh, map[string]string{"country": pend.country, "language": pend.language})
}

// exchangeCode trades an authorization code for tokens and returns the refresh.
func (p *Provider) exchangeCode(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"client_id":     {clientID},
	}.Encode()
	basic := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if _, err := p.client.PostForm(ctx, authAPI+"/connect/token", form, map[string]string{
		"Authorization": "Basic " + basic,
		"Accept":        "application/json",
	}, &resp); err != nil {
		return "", fmt.Errorf("%w: token exchange failed: %v", provider.ErrCredentialsRejected, err)
	}
	if resp.RefreshToken == "" {
		return "", fmt.Errorf("%w: token exchange returned no refresh_token", provider.ErrCredentialsRejected)
	}
	return resp.RefreshToken, nil
}

// parseCode accepts either a bare code or the whole callback URL/query and
// returns the code value.
func parseCode(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// A pasted URL (or a bare query with code=...): pull out the code parameter.
	if i := strings.IndexAny(s, "?#"); i >= 0 || strings.Contains(s, "code=") {
		frag := s
		if i >= 0 {
			frag = s[i+1:]
		}
		if v, err := url.ParseQuery(frag); err == nil {
			if c := strings.TrimSpace(v.Get("code")); c != "" {
				return c
			}
		}
	}
	// Otherwise treat the whole thing as the code, minus any stray query tail.
	if i := strings.IndexAny(s, "&? "); i >= 0 {
		s = s[:i]
	}
	return s
}

func (p *Provider) putPending(token string, pc *pendingConnect) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if p.pending == nil {
		p.pending = map[string]*pendingConnect{}
	}
	for k, v := range p.pending {
		if time.Since(v.createdAt) > pendingTTL {
			delete(p.pending, k)
		}
	}
	p.pending[token] = pc
}

func (p *Provider) takePending(token string) *pendingConnect {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	pc := p.pending[token]
	if pc == nil {
		return nil
	}
	delete(p.pending, token)
	if time.Since(pc.createdAt) > pendingTTL {
		return nil
	}
	return pc
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
