// Package lidl reads receipts ("tickets") from Lidl Plus.
//
// Lidl publishes no API for this. The endpoints and headers below are the ones
// the Lidl Plus mobile app uses, as documented by the community project
// github.com/Andre0512/lidl-plus. They can change without notice.
//
// Logging in the way the app does needs a real browser (reCAPTCHA + Akamai)
// and an SMS code, which does not belong in an MCP server, so this provider
// takes the refresh token that flow produces and keeps it alive on its own.
// The tools/lidl-login helper runs that one-time browser login locally and
// prints the refresh token to paste here.
package lidl

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/httpx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/secret"
)

// ID is the provider id used in tool arguments.
const ID = "lidl"

// The app's own OAuth client and endpoints.
const (
	clientID     = "LidlPlusNativeClient"
	clientSecret = "secret"
	authAPI      = "https://accounts.lidl.com"
	ticketAPI    = "https://tickets.lidlplus.com/api/v2"
	appPackage   = "com.lidl.eci.lidl.plus"
	appVersion   = "999.99.9"
	operatingSys = "iOs"
)

// maxPages caps the paging loop so a surprising totalCount cannot spin forever.
const maxPages = 100

// Provider is the Lidl Plus receipt source.
type Provider struct {
	client *httpx.Client
	store  *secret.Store

	defaultCountry  string
	defaultLanguage string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	// pending holds browser-login sessions between the "start" call (which opens
	// the Lidl login in the user's browser) and the "resume" call (which brings
	// back the OAuth code), keyed by the continuation token handed to the caller.
	pendingMu sync.Mutex
	pending   map[string]*pendingConnect
}

// New builds the provider. defaultCountry/defaultLanguage are used when the
// stored credentials carry none.
func New(client *httpx.Client, store *secret.Store, defaultCountry, defaultLanguage string) *Provider {
	return &Provider{
		client:          client,
		store:           store,
		defaultCountry:  strings.ToUpper(defaultCountry),
		defaultLanguage: strings.ToLower(defaultLanguage),
	}
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return ID }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Lidl Plus" }

// Status implements provider.Provider.
func (p *Provider) Status(ctx context.Context) provider.Status {
	status := provider.Status{
		Provider:    ID,
		DisplayName: p.DisplayName(),
		Sources:     []string{"api"},
		RequiredFields: []provider.Field{
			{Name: "country", Description: "two letter country of the Lidl Plus account, e.g. PL, DE, NL (default PL)", Required: false},
			{Name: "language", Description: "interface language for item names, e.g. pl, de, en", Required: false},
			{Name: "refresh_token", Description: "optional: paste a Lidl Plus refresh token if you already have one; leave empty to log in through your browser", Required: false, Secret: true},
		},
		Notes: []string{
			"unofficial app API; item lines, discounts and taxes are complete when it answers",
			"Connect logs you in through your own browser: a Lidl login tab opens (solve the captcha + SMS there), then paste back the code it shows — no local tooling needed",
			"the server keeps the resulting refresh token alive on its own",
		},
	}
	status.StoredFields = p.store.FieldNames(ID)
	status.LoggedIn = p.store.Field(ID, "refresh_token") != ""
	if status.LoggedIn && p.country() == "" {
		status.Error = "no country stored; call receipts_login again with the country field"
	}
	return status
}

// Login stores a Lidl Plus refresh token (produced by the tools/lidl-login
// helper) with its country/language and exchanges it once so a bad token is
// reported now. Lidl's login is reCAPTCHA- and SMS-gated, so the browser part
// is done off to the side, exactly as the app does it.
func (p *Provider) Login(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	// Resuming the browser login with the pasted OAuth code.
	if cont := strings.TrimSpace(fields["continuation"]); cont != "" {
		return p.resumeConnect(ctx, cont, fields)
	}
	// A pasted refresh token (e.g. from tools/lidl-login) skips the browser step.
	if token := strings.TrimSpace(fields["refresh_token"]); token != "" {
		return p.storeAndVerify(ctx, token, fields)
	}
	// Otherwise begin the browser login: open the Lidl OAuth page in the user's
	// own browser (where reCAPTCHA and SMS work), then ask for the code back.
	return p.startConnect(ctx, fields)
}

// storeAndVerify persists a refresh token with the resolved country/language and
// exchanges it once to confirm it works.
func (p *Provider) storeAndVerify(ctx context.Context, token string, fields map[string]string) (provider.LoginResult, error) {
	country := strings.ToUpper(strings.TrimSpace(fields["country"]))
	if country == "" {
		country = p.defaultCountry
	}
	if country == "" {
		return provider.LoginResult{}, errors.New("lidl: country is required (e.g. PL)")
	}
	language := strings.ToLower(strings.TrimSpace(fields["language"]))
	if language == "" {
		language = p.defaultLanguage
	}
	if language == "" {
		language = strings.ToLower(country)
	}

	if err := p.store.Merge(ID, map[string]string{
		"refresh_token": token,
		"country":       country,
		"language":      language,
	}); err != nil {
		return provider.LoginResult{}, err
	}

	if _, err := p.token(ctx); err != nil {
		return provider.LoginResult{}, fmt.Errorf("lidl: stored token was rejected: %w", err)
	}

	return provider.LoginResult{
		Provider: ID,
		OK:       true,
		Message:  fmt.Sprintf("Lidl Plus ready for country %s", country),
		Stored:   p.store.FieldNames(ID),
		Notes:    []string{"the refresh token rotates on every renewal and is re-saved automatically"},
	}, nil
}

// Logout implements provider.Provider.
func (p *Provider) Logout(ctx context.Context) error {
	p.mu.Lock()
	p.accessToken, p.expiresAt = "", time.Time{}
	p.mu.Unlock()
	return p.store.Delete(ID)
}

// List implements provider.Provider. Lidl pages tickets newest first; we stop
// as soon as the page falls out of the requested window.
func (p *Provider) List(ctx context.Context, q provider.Query) ([]receipt.Receipt, error) {
	q = q.Normalize()
	country := p.country()
	if country == "" {
		return nil, provider.ErrNotLoggedIn
	}

	var out []receipt.Receipt
	for page := 1; page <= maxPages; page++ {
		var payload ticketPage
		endpoint := fmt.Sprintf("%s/%s/tickets?pageNumber=%d&onlyFavorite=false", ticketAPI, url.PathEscape(country), page)
		if _, err := p.get(ctx, endpoint, &payload); err != nil {
			return nil, err
		}
		if len(payload.Tickets) == 0 {
			break
		}

		reachedOlder := false
		for _, t := range payload.Tickets {
			r := t.toReceipt()
			if !q.From.IsZero() && r.PurchasedAt.Before(q.From) {
				reachedOlder = true
				continue
			}
			if !q.To.IsZero() && r.PurchasedAt.After(q.To) {
				continue
			}
			out = append(out, r)
			if len(out) >= q.Limit {
				return out, nil
			}
		}
		if reachedOlder || !payload.hasMore(page) {
			break
		}
	}
	receipt.SortByDate(out)
	return out, nil
}

// Get implements provider.Provider.
func (p *Provider) Get(ctx context.Context, id string) (receipt.Receipt, error) {
	country := p.country()
	if country == "" {
		return receipt.Receipt{}, provider.ErrNotLoggedIn
	}
	endpoint := fmt.Sprintf("%s/%s/tickets/%s", ticketAPI, url.PathEscape(country), url.PathEscape(id))

	var detail ticketDetail
	raw, err := p.get(ctx, endpoint, &detail)
	if err != nil {
		return receipt.Receipt{}, err
	}
	r := detail.toReceipt()
	r.Raw = json.RawMessage(raw)
	return r, nil
}

// get performs an authenticated GET, renewing the access token once if the
// API rejects it mid-flight.
func (p *Provider) get(ctx context.Context, endpoint string, out any) ([]byte, error) {
	token, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := p.client.GetJSON(ctx, endpoint, p.headers(token), out)
	var statusErr *httpx.StatusError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.Unauthorized():
			p.invalidateToken()
			token, tokenErr := p.token(ctx)
			if tokenErr != nil {
				return nil, tokenErr
			}
			return p.client.GetJSON(ctx, endpoint, p.headers(token), out)
		case statusErr.NotFound():
			return nil, fmt.Errorf("%w: %s", provider.ErrEndpointUnavailable, statusErr.Error())
		}
	}
	return raw, err
}

func (p *Provider) headers(token string) map[string]string {
	return map[string]string{
		"Authorization":    "Bearer " + token,
		"App-Version":      appVersion,
		"Operating-System": operatingSys,
		"App":              appPackage,
		"Accept-Language":  p.language(),
	}
}

// token returns a valid access token, renewing it from the refresh token when
// the current one is missing or about to expire.
func (p *Provider) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.accessToken != "" && time.Now().Before(p.expiresAt.Add(-30*time.Second)) {
		return p.accessToken, nil
	}

	refresh := p.store.Field(ID, "refresh_token")
	if refresh == "" {
		return "", provider.ErrNotLoggedIn
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}.Encode()
	basic := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))

	var resp tokenResponse
	_, err := p.client.PostForm(ctx, authAPI+"/connect/token", form, map[string]string{
		"Authorization": "Basic " + basic,
		"Accept":        "application/json",
	}, &resp)
	if err != nil {
		var statusErr *httpx.StatusError
		if errors.As(err, &statusErr) && (statusErr.Unauthorized() || statusErr.StatusCode == 400) {
			return "", fmt.Errorf("%w: %s", provider.ErrCredentialsRejected, statusErr.Error())
		}
		return "", err
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("%w: token endpoint returned no access_token", provider.ErrCredentialsRejected)
	}

	p.accessToken = resp.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)

	// Lidl rotates the refresh token on every renewal; losing the new one
	// means the next start has to go through the browser flow again.
	if resp.RefreshToken != "" && resp.RefreshToken != refresh {
		if err := p.store.Merge(ID, map[string]string{"refresh_token": resp.RefreshToken}); err != nil {
			return "", fmt.Errorf("lidl: could not persist the rotated refresh token: %w", err)
		}
	}
	return p.accessToken, nil
}

func (p *Provider) invalidateToken() {
	p.mu.Lock()
	p.accessToken, p.expiresAt = "", time.Time{}
	p.mu.Unlock()
}

func (p *Provider) country() string {
	if c := p.store.Field(ID, "country"); c != "" {
		return strings.ToUpper(c)
	}
	return p.defaultCountry
}

func (p *Provider) language() string {
	if l := p.store.Field(ID, "language"); l != "" {
		return l
	}
	if p.defaultLanguage != "" {
		return p.defaultLanguage
	}
	return "en"
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

type ticketPage struct {
	Tickets    []ticketSummary `json:"tickets"`
	Size       int             `json:"size"`
	TotalCount int             `json:"totalCount"`
	Page       int             `json:"page"`
}

func (t ticketPage) hasMore(page int) bool {
	if t.Size <= 0 || t.TotalCount <= 0 {
		return false
	}
	return page*t.Size < t.TotalCount
}

type currency struct {
	Code   string `json:"code"`
	Symbol string `json:"symbol"`
}

type ticketSummary struct {
	ID            string   `json:"id"`
	Date          string   `json:"date"`
	TotalAmount   string   `json:"totalAmount"`
	StoreCode     string   `json:"storeCode"`
	Currency      currency `json:"currency"`
	ArticlesCount int      `json:"articlesCount"`
	IsFavorite    bool     `json:"isFavorite"`
	SequenceNo    string   `json:"sequenceNumber"`
}

func (t ticketSummary) toReceipt() receipt.Receipt {
	code := strings.ToUpper(t.Currency.Code)
	r := receipt.Receipt{
		Provider:    ID,
		ID:          t.ID,
		Source:      receipt.SourceAPI,
		PurchasedAt: parseTime(t.Date),
		Currency:    code,
		Total:       money.ParseOrZero(t.TotalAmount, code),
		Number:      t.SequenceNo,
	}
	if t.StoreCode != "" {
		r.Store = &receipt.Store{Name: "Lidl " + t.StoreCode, Code: t.StoreCode}
	}
	if t.ArticlesCount > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d article(s); call receipts_get for the item lines", t.ArticlesCount))
	}
	return r
}

type ticketDetail struct {
	ID           string          `json:"id"`
	Date         string          `json:"date"`
	TotalAmount  string          `json:"totalAmount"`
	StoreCode    string          `json:"storeCode"`
	Currency     currency        `json:"currency"`
	SequenceNo   string          `json:"sequenceNumber"`
	BarCode      string          `json:"barCode"`
	ItemsLine    []ticketItem    `json:"itemsLine"`
	Taxes        []ticketTax     `json:"taxes"`
	CouponsUsed  []ticketCoupon  `json:"couponsUsed"`
	Payments     []ticketPayment `json:"payments"`
	TenderChange []ticketPayment `json:"tenderChange"`
	Store        *ticketStore    `json:"store"`
}

type ticketStore struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Street   string `json:"street"`
	Locality string `json:"locality"`
	Postcode string `json:"postCode"`
	Country  string `json:"countryCode"`
}

type ticketItem struct {
	Name             string           `json:"name"`
	Quantity         string           `json:"quantity"`
	IsWeight         bool             `json:"isWeight"`
	CurrentUnitPrice string           `json:"currentUnitPrice"`
	OriginalAmount   string           `json:"originalAmount"`
	TaxGroup         string           `json:"taxGroup"`
	TaxGroupName     string           `json:"taxGroupName"`
	CodeInput        string           `json:"codeInput"`
	Deposit          *string          `json:"deposit"`
	Discounts        []ticketDiscount `json:"discounts"`
	UnitOfMeasure    string           `json:"unitOfMeasure"`
}

type ticketDiscount struct {
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

type ticketTax struct {
	TaxGroupName  string `json:"taxGroupName"`
	Percentage    string `json:"percentage"`
	Amount        string `json:"amount"`
	NetAmount     string `json:"netAmount"`
	TaxableAmount string `json:"taxableAmount"`
}

type ticketCoupon struct {
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

type ticketPayment struct {
	Description    string `json:"description"`
	Type           string `json:"type"`
	Amount         string `json:"amount"`
	RoundingAmount string `json:"roundingAmount"`
}

func (t ticketDetail) toReceipt() receipt.Receipt {
	code := strings.ToUpper(t.Currency.Code)
	r := receipt.Receipt{
		Provider:      ID,
		ID:            t.ID,
		Source:        receipt.SourceAPI,
		PurchasedAt:   parseTime(t.Date),
		Currency:      code,
		Total:         money.ParseOrZero(t.TotalAmount, code),
		Number:        firstNonEmpty(t.SequenceNo, t.BarCode),
		ItemsComplete: true,
	}

	switch {
	case t.Store != nil:
		r.Store = &receipt.Store{
			Name:    firstNonEmpty(t.Store.Name, "Lidl "+t.Store.ID),
			Code:    firstNonEmpty(t.Store.ID, t.StoreCode),
			Street:  t.Store.Street,
			City:    strings.TrimSpace(strings.Join(nonEmpty(t.Store.Postcode, t.Store.Locality), " ")),
			Country: t.Store.Country,
		}
	case t.StoreCode != "":
		r.Store = &receipt.Store{Name: "Lidl " + t.StoreCode, Code: t.StoreCode}
	}

	for _, item := range t.ItemsLine {
		r.Items = append(r.Items, item.toItem(code))
	}
	for _, tax := range t.Taxes {
		r.Taxes = append(r.Taxes, receipt.Tax{
			Group:  tax.TaxGroupName,
			Rate:   tax.Percentage,
			Net:    money.ParseOrZero(firstNonEmpty(tax.NetAmount, tax.TaxableAmount), code),
			Amount: money.ParseOrZero(tax.Amount, code),
		})
	}
	for _, coupon := range t.CouponsUsed {
		r.Discounts = append(r.Discounts, receipt.Discount{
			Description: coupon.Description,
			Amount:      money.ParseOrZero(coupon.Amount, code).Abs(),
		})
	}
	for _, payment := range append(append([]ticketPayment{}, t.Payments...), t.TenderChange...) {
		amount := money.ParseOrZero(payment.Amount, code)
		if amount.IsZero() && payment.Description == "" {
			continue
		}
		r.Payments = append(r.Payments, receipt.Payment{
			Method: firstNonEmpty(payment.Description, payment.Type),
			Amount: amount,
		})
	}
	return r
}

func (i ticketItem) toItem(code string) receipt.Item {
	gross := money.ParseOrZero(i.OriginalAmount, code)
	item := receipt.Item{
		Name:      strings.TrimSpace(i.Name),
		Quantity:  parseQuantity(i.Quantity),
		IsWeight:  i.IsWeight,
		Unit:      i.UnitOfMeasure,
		UnitPrice: money.ParseOrZero(i.CurrentUnitPrice, code),
		Gross:     gross,
		Code:      i.CodeInput,
		TaxGroup:  firstNonEmpty(i.TaxGroupName, i.TaxGroup),
	}

	total := gross
	for _, d := range i.Discounts {
		amount := money.ParseOrZero(d.Amount, code).Abs()
		item.Discounts = append(item.Discounts, receipt.Discount{Description: d.Description, Amount: amount})
		total = total.Sub(amount)
	}
	item.Total = total

	if i.Deposit != nil && strings.TrimSpace(*i.Deposit) != "" {
		deposit := money.ParseOrZero(*i.Deposit, code)
		item.Deposit = &deposit
	}
	return item
}

// timeLayouts covers the shapes the ticket API has been seen to use.
var timeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseQuantity reads "1", "0,404" or "2.000" as a number.
func parseQuantity(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
