package browserlogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// LidlDriver performs the Lidl Plus login the way the app does: OAuth
// Authorization Code + PKCE against accounts.lidl.com, driven in a headless
// browser, with the SMS 2FA step relayed to the user through the login page.
// The authorization code is captured from the app-scheme redirect
// (com.lidlplus.app://callback?code=…) and exchanged for a refresh token.
//
// EXPERIMENTAL: the browser steps are written against the login form the
// community client github.com/Andre0512/lidl-plus documents; Lidl can change
// their markup at any time. When a step cannot be driven, the driver fails with
// a message pointing the user at the paste path, which is always available.
type LidlDriver struct {
	chromiumPath string
	httpClient   *http.Client
}

// Lidl OAuth constants, as recovered from the app (see docs/reverse-engineering.md).
const (
	lidlClientID     = "LidlPlusNativeClient"
	lidlClientSecret = "secret"
	lidlAuthBase     = "https://accounts.lidl.com"
	lidlRedirectURI  = "com.lidlplus.app://callback"
	lidlScope        = "openid profile offline_access lpprofile lpapis"
)

// Selectors the driver types into. Kept together so a markup change is a
// one-place edit.
var lidlSel = struct {
	welcomeLogin string
	emailOrPhone string
	submitEmail  string
	password     string
	submitLogin  string
	smsCode      string
	submitSMS    string
}{
	welcomeLogin: "#button_welcome_login",
	emailOrPhone: "input[name='EmailOrPhone']",
	submitEmail:  "#button_btn_submit_email",
	password:     "#field_Password",
	submitLogin:  "#button_submit",
	smsCode:      "input[name='VerificationCode']",
	submitSMS:    ".role_next",
}

// NewLidlDriver builds a Lidl browser-login driver. chromiumPath may be empty to
// let chromedp find a browser on PATH.
func NewLidlDriver(chromiumPath string) *LidlDriver {
	return &LidlDriver{
		chromiumPath: chromiumPath,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

// Provider implements Driver.
func (d *LidlDriver) Provider() string { return "lidl" }

// Run implements Driver.
func (d *LidlDriver) Run(ctx context.Context, ask Prompt) (map[string]string, string, error) {
	creds, err := ask(Form{
		Title: "Log in to Lidl Plus",
		Note:  "Enter your Lidl Plus phone number and password. Lidl will text you a code next.",
		Fields: []Field{
			{Name: "phone", Label: "Phone (with country code, e.g. +48…)", Type: "tel", Required: true},
			{Name: "password", Label: "Password", Type: "password", Required: true},
			{Name: "country", Label: "Country (e.g. PL)", Type: "text", Placeholder: "PL", Required: true},
			{Name: "language", Label: "Language (optional)", Type: "text", Placeholder: "pl"},
		},
	})
	if err != nil {
		return nil, "", err
	}
	phone := strings.TrimSpace(creds["phone"])
	password := creds["password"]
	country := strings.ToUpper(strings.TrimSpace(creds["country"]))
	language := strings.ToLower(strings.TrimSpace(creds["language"]))
	if language == "" {
		language = strings.ToLower(country)
	}
	if phone == "" || password == "" || country == "" {
		return nil, "", fmt.Errorf("phone, password and country are required")
	}

	verifier, challenge, err := pkce()
	if err != nil {
		return nil, "", err
	}
	authURL := d.authorizeURL(challenge, country, language)

	allocCtx, cancelAlloc := d.browser(ctx)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Capture the authorization code from the app-scheme redirect.
	codeCh := make(chan string, 1)
	chromedp.ListenTarget(browserCtx, func(ev any) {
		if e, ok := ev.(*network.EventRequestWillBeSent); ok {
			if code := codeFromRedirect(e.Request.URL); code != "" {
				select {
				case codeCh <- code:
				default:
				}
			}
		}
	})

	// Drive phone + password.
	if err := chromedp.Run(browserCtx,
		network.Enable(),
		chromedp.Navigate(authURL),
		clickIfPresent(lidlSel.welcomeLogin),
		waitAndType(lidlSel.emailOrPhone, phone),
		clickIfPresent(lidlSel.submitEmail),
		waitAndType(lidlSel.password, password),
		chromedp.Click(lidlSel.submitLogin, chromedp.ByQuery),
	); err != nil {
		return nil, "", fmt.Errorf("lidl login (phone/password) could not be driven: %w — use the paste path instead", err)
	}

	// If the code already arrived (no 2FA), skip the SMS step.
	code := waitForCode(ctx, codeCh, 4*time.Second)
	if code == "" {
		smsValues, err := ask(Form{
			Title:  "Enter the SMS code",
			Note:   "Lidl just texted a verification code to " + phone + ".",
			Fields: []Field{{Name: "code", Label: "SMS code", Type: "text", Required: true}},
		})
		if err != nil {
			return nil, "", err
		}
		smsCode := strings.TrimSpace(smsValues["code"])
		if smsCode == "" {
			return nil, "", fmt.Errorf("no SMS code entered")
		}
		if err := chromedp.Run(browserCtx,
			waitAndType(lidlSel.smsCode, smsCode),
			chromedp.Click(lidlSel.submitSMS, chromedp.ByQuery),
		); err != nil {
			return nil, "", fmt.Errorf("submitting the SMS code failed: %w", err)
		}
		code = waitForCode(ctx, codeCh, 15*time.Second)
	}
	if code == "" {
		return nil, "", fmt.Errorf("did not receive an authorization code from Lidl (login may have failed) — use the paste path instead")
	}

	refresh, err := d.exchange(ctx, code, verifier)
	if err != nil {
		return nil, "", err
	}
	return map[string]string{
		"refresh_token": refresh,
		"country":       country,
		"language":      language,
	}, "Lidl Plus connected for " + country, nil
}

// browser builds a chromedp allocator, honouring a configured Chromium path.
func (d *LidlDriver) browser(ctx context.Context) (context.Context, context.CancelFunc) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.Headless, chromedp.NoSandbox)
	if d.chromiumPath != "" {
		opts = append(opts, chromedp.ExecPath(d.chromiumPath))
	}
	return chromedp.NewExecAllocator(ctx, opts...)
}

func (d *LidlDriver) authorizeURL(challenge, country, language string) string {
	q := url.Values{
		"client_id":             {lidlClientID},
		"response_type":         {"code"},
		"scope":                 {lidlScope},
		"redirect_uri":          {lidlRedirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"Country":               {country},
		"language":              {language + "-" + country},
	}
	return lidlAuthBase + "/connect/authorize?" + q.Encode()
}

// exchange trades the authorization code for a refresh token.
func (d *LidlDriver) exchange(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {lidlRedirectURI},
		"code_verifier": {verifier},
	}.Encode()
	basic := base64.StdEncoding.EncodeToString([]byte(lidlClientID + ":" + lidlClientSecret))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lidlAuthBase+"/connect/token", strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Accept", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("token exchange response was not JSON: %w", err)
	}
	if body.RefreshToken == "" {
		return "", fmt.Errorf("token exchange returned no refresh token (%s)", body.Error)
	}
	return body.RefreshToken, nil
}

// --- helpers ---

func codeFromRedirect(rawURL string) string {
	if !strings.HasPrefix(rawURL, "com.lidlplus.app://") && !strings.Contains(rawURL, "/callback") {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("code")
}

func waitForCode(ctx context.Context, ch <-chan string, d time.Duration) string {
	select {
	case code := <-ch:
		return code
	case <-time.After(d):
		return ""
	case <-ctx.Done():
		return ""
	}
}

func clickIfPresent(sel string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		// Best-effort: the welcome/login button is not on every entry page.
		clickCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		_ = chromedp.Run(clickCtx, chromedp.Click(sel, chromedp.ByQuery))
		return nil
	})
}

func waitAndType(sel, value string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := chromedp.WaitVisible(sel, chromedp.ByQuery).Do(ctx); err != nil {
			return fmt.Errorf("field %q did not appear: %w", sel, err)
		}
		return chromedp.SendKeys(sel, value, chromedp.ByQuery).Do(ctx)
	})
}

// pkce returns a PKCE verifier and its S256 challenge.
func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}
