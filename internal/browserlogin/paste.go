package browserlogin

import (
	"context"
	"fmt"
	"strings"
)

// PasteDriver is the universal, always-available login path: it shows one form
// asking for the credential the provider actually needs (a Lidl refresh token,
// an Action refresh/access token, an Allegro session cookie) and stores what the
// user pastes. It needs no browser, so it works everywhere and is the reliable
// fallback when the browser-driven path is unavailable or blocked (Allegro's
// DataDome, a missing Chromium).
type PasteDriver struct {
	provider string
	form     Form
	// normalize maps the submitted values to the fields to store; nil stores
	// the values as-is.
	normalize func(map[string]string) (map[string]string, error)
}

// Provider implements Driver.
func (d *PasteDriver) Provider() string { return d.provider }

// Form returns the single form this driver shows, for rendering on the page.
func (d *PasteDriver) Form() Form { return d.form }

// Run implements Driver.
func (d *PasteDriver) Run(ctx context.Context, ask Prompt) (map[string]string, string, error) {
	values, err := ask(d.form)
	if err != nil {
		return nil, "", err
	}
	store := values
	if d.normalize != nil {
		store, err = d.normalize(values)
		if err != nil {
			return nil, "", err
		}
	}
	trimmed := map[string]string{}
	for k, v := range store {
		v = strings.TrimSpace(v)
		if v != "" {
			trimmed[k] = v
		}
	}
	if len(trimmed) == 0 {
		return nil, "", fmt.Errorf("nothing was entered")
	}
	return trimmed, "credentials saved", nil
}

// LidlPaste asks for a Lidl refresh token plus country.
func LidlPaste() *PasteDriver {
	return &PasteDriver{
		provider: "lidl",
		form: Form{
			Title: "Connect Lidl Plus",
			Note:  "Paste a Lidl Plus refresh token (see README: obtaining a Lidl Plus refresh token). Or use the guided login on the Lidl card to get one automatically.",
			Fields: []Field{
				{Name: "refresh_token", Label: "Refresh token", Type: "password", Required: true},
				{Name: "country", Label: "Country (e.g. PL)", Type: "text", Placeholder: "PL", Required: true},
				{Name: "language", Label: "Language (optional, e.g. pl)", Type: "text", Placeholder: "pl"},
			},
		},
	}
}

// ActionPaste asks for an Action refresh or access token.
func ActionPaste() *PasteDriver {
	return &PasteDriver{
		provider: "action",
		form: Form{
			Title: "Connect Action",
			Note:  "Log in to Mijn Action (or proxy the app) and copy the OAuth refresh token, or the Authorization bearer token from a request to gateway.action.com. See docs/reverse-engineering.md.",
			Fields: []Field{
				{Name: "refresh_token", Label: "Refresh token (preferred)", Type: "password"},
				{Name: "token", Label: "…or an access token", Type: "password"},
			},
		},
		normalize: func(v map[string]string) (map[string]string, error) {
			if strings.TrimSpace(v["refresh_token"]) == "" && strings.TrimSpace(v["token"]) == "" {
				return nil, fmt.Errorf("enter a refresh token or an access token")
			}
			return v, nil
		},
	}
}

// AllegroPaste asks for the allegro.pl session cookie. This is the recommended
// path for Allegro because its site runs DataDome bot protection that blocks
// automated login.
func AllegroPaste() *PasteDriver {
	return &PasteDriver{
		provider: "allegro",
		form: Form{
			Title: "Connect Allegro",
			Note:  "Log in to allegro.pl in your browser, open the developer tools, and copy the whole Cookie header of any request to api.allegro.pl (the QXLSESSID value is the one that matters).",
			Fields: []Field{
				{Name: "cookie", Label: "Cookie header", Type: "password", Required: true},
			},
		},
	}
}
