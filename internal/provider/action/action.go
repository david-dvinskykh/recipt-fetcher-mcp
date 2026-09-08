// Package action reads digital receipts from a Mijn Action account.
//
// Action publishes no API for this, but the Android app's own mechanism was
// recovered by decompiling it (see docs/reverse-engineering.md). It is not a
// REST endpoint: the app talks GraphQL to a single gateway, authenticated by an
// OAuth2 bearer token from Action's identity provider (SAP Gigya CDC, via
// AppAuth / OpenID Connect with PKCE). The exact receipt queries below are the
// ones the app sends.
//
// Logging in the OAuth way needs a browser (Gigya's hosted login), which does
// not belong in an MCP server, so — like Lidl — this provider takes the tokens
// that flow produces: either a ready access token, or a refresh token it renews
// against the Gigya OIDC token endpoint on its own.
package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/httpx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/jsonx"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/money"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/provider"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/receipt"
	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/secret"
)

// ID is the provider id used in tool arguments.
const ID = "action"

// The app's own endpoints, recovered from the APK.
const (
	// gatewayURL is the production GraphQL gateway; the app also ships an
	// "integration-gateway.action.com" for staging.
	gatewayURL = "https://gateway.action.com/api/gateway"
	// gigyaIssuer is the SAP Gigya CDC OpenID Connect issuer the app logs in
	// against; its token endpoint is <issuer>/token.
	gigyaIssuer = "https://fidm.eu1.gigya.com/oidc/op/v1.0/4_M_vWUHGO9oyocs0oKu8m7Q"
	// defaultClientID is the Gigya OIDC client the app identifies as. It can be
	// overridden if the refresh flow rejects it.
	defaultClientID = "4_M_vWUHGO9oyocs0oKu8m7Q"

	receiptsURL = "https://www.action.com/nl-nl/mijn-action/"
)

// The two receipt operations, verbatim from the app.
const (
	listQuery = `query GetReceipts($limit: Int, $offset: String) { receiptList(limit: $limit, offset: $offset) { receipts { id dateTime store { name id } price { currency total } returningPeriod { returnable } } offset } }`

	detailQuery = `query GetSingleReceipt($receiptId: String!) { receipt(id: $receiptId) { receiptNumber dateTime store { address } returningPeriod { totalDays remainingDays lastReturnDate } price { currency total subTotal employeeDiscount otherDiscounts vat { total { vatAmount totalIncludingVat totalExcludingVat } perPercentage { percentage specification { vatAmount totalIncludingVat totalExcludingVat } } } } totalQuantity barcode qrCode products { code description totalPrice price { adjustedSalesPrice regularSalesPrice } quantity } paymentMethods membershipId warrantyYears } }`
)

// Provider is the Action digital receipt source.
type Provider struct {
	client *httpx.Client
	store  *secret.Store

	// tokenEndpoint is the Gigya OIDC token endpoint; a field so tests can point
	// it at a stub. Empty means the real one derived from gigyaIssuer.
	tokenEndpoint string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New builds the provider.
func New(client *httpx.Client, store *secret.Store) *Provider {
	return &Provider{client: client, store: store}
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return ID }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Action" }

// Status implements provider.Provider.
func (p *Provider) Status(ctx context.Context) provider.Status {
	status := provider.Status{
		Provider:     ID,
		DisplayName:  p.DisplayName(),
		StoredFields: p.store.FieldNames(ID),
		Sources:      []string{"api"},
		RequiredFields: []provider.Field{
			{Name: "refresh_token", Description: "OAuth refresh token from a Mijn Action (Gigya) login; renewed automatically (see README)", Required: false, Secret: true},
			{Name: "token", Description: "a ready access token captured from an app session, instead of a refresh token", Required: false, Secret: true},
			{Name: "client_id", Description: "Gigya OIDC client id for the refresh; defaults to the app's own", Required: false},
			{Name: "endpoint", Description: "override for the GraphQL gateway; defaults to " + gatewayURL, Required: false},
		},
		Notes: []string{
			"reverse-engineered from the Action app: GraphQL gateway + Gigya OIDC login (see docs/reverse-engineering.md)",
			"gives the full receipt: item lines, VAT breakdown and payment methods",
		},
	}
	status.LoggedIn = p.hasCredentials()
	return status
}

// Login stores tokens and verifies them with one listing when it can.
func (p *Provider) Login(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	update := map[string]string{}
	for _, name := range []string{"refresh_token", "token", "client_id", "endpoint"} {
		if value := strings.TrimSpace(fields[name]); value != "" {
			update[name] = value
		}
	}
	if update["refresh_token"] == "" && update["token"] == "" &&
		p.store.Field(ID, "refresh_token") == "" && p.store.Field(ID, "token") == "" {
		return provider.LoginResult{}, errors.New("action: a refresh_token or a token is required (from a Mijn Action login; see README)")
	}
	if err := p.store.Merge(ID, update); err != nil {
		return provider.LoginResult{}, err
	}
	p.invalidateToken()

	result := provider.LoginResult{Provider: ID, OK: true, Stored: p.store.FieldNames(ID)}
	receipts, err := p.list(ctx, provider.Query{Limit: 1})
	if err != nil {
		result.OK = false
		result.Message = fmt.Sprintf("credentials stored, but the gateway did not accept them: %v", err)
		result.Notes = append(result.Notes, "listings will fall back to e-mail until this is fixed")
		return result, nil
	}
	result.Message = fmt.Sprintf("Action gateway ready (%d receipt(s) on the first page)", len(receipts))
	return result, nil
}

// Logout implements provider.Provider.
func (p *Provider) Logout(ctx context.Context) error {
	p.invalidateToken()
	return p.store.Delete(ID)
}

// List implements provider.Provider.
func (p *Provider) List(ctx context.Context, q provider.Query) ([]receipt.Receipt, error) {
	return p.list(ctx, q.Normalize())
}

func (p *Provider) list(ctx context.Context, q provider.Query) ([]receipt.Receipt, error) {
	var (
		out    []receipt.Receipt
		offset string
	)
	for page := 0; page < 50; page++ {
		vars := map[string]any{"limit": pageSize(q.Limit)}
		if offset != "" {
			vars["offset"] = offset
		}
		data, err := p.graphql(ctx, "GetReceipts", listQuery, vars)
		if err != nil {
			return nil, err
		}
		list := data.Object("receiptList")
		if list == nil {
			return nil, fmt.Errorf("%w: response had no receiptList", provider.ErrEndpointUnavailable)
		}
		entries, _ := list.FindArray("receipts")
		for _, e := range entries {
			r := listReceipt(e)
			if r.ID == "" || !r.InRange(q.From, q.To) {
				continue
			}
			out = append(out, r)
			if len(out) >= q.Limit {
				receipt.SortByDate(out)
				return out, nil
			}
		}
		next := list.String("offset")
		if next == "" || next == offset || len(entries) == 0 {
			break
		}
		offset = next
	}
	receipt.SortByDate(out)
	return out, nil
}

// Get implements provider.Provider, returning the full receipt.
func (p *Provider) Get(ctx context.Context, id string) (receipt.Receipt, error) {
	data, err := p.graphql(ctx, "GetSingleReceipt", detailQuery, map[string]any{"receiptId": id})
	if err != nil {
		return receipt.Receipt{}, err
	}
	obj := data.Object("receipt")
	if obj == nil {
		return receipt.Receipt{}, fmt.Errorf("action: receipt %q not found", id)
	}
	r := detailReceipt(id, obj)
	raw, _ := json.Marshal(obj)
	r.Raw = raw
	return r, nil
}

// graphql posts one operation and returns the `data` object, mapping transport
// and GraphQL-level errors onto the provider's sentinels.
func (p *Provider) graphql(ctx context.Context, op, query string, vars map[string]any) (jsonx.Object, error) {
	token, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := p.store.Field(ID, "endpoint")
	if endpoint == "" {
		endpoint = gatewayURL
	}
	payload := map[string]any{"operationName": op, "query": query, "variables": vars}
	headers := map[string]string{
		"Authorization":             "Bearer " + token,
		"apollographql-client-name": "action-consumerapp-android",
		"x-client-type":             "android",
	}

	raw, err := p.client.PostJSON(ctx, endpoint, payload, headers, nil)
	if err != nil {
		var statusErr *httpx.StatusError
		if errors.As(err, &statusErr) {
			if statusErr.Unauthorized() {
				p.invalidateToken()
				return nil, fmt.Errorf("%w: %s", provider.ErrCredentialsRejected, statusErr.Error())
			}
			if statusErr.NotFound() {
				return nil, fmt.Errorf("%w: %s", provider.ErrEndpointUnavailable, statusErr.Error())
			}
		}
		return nil, err
	}
	body, err := jsonx.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: gateway response was not JSON: %v", provider.ErrEndpointUnavailable, err)
	}
	if errs, _ := body.Array("errors"); len(errs) > 0 {
		// An auth error inside a 200 GraphQL body is common; treat "unauth" text
		// as a credential problem so the mailbox fallback takes over.
		text := errorText(errs)
		low := strings.ToLower(text)
		if strings.Contains(low, "unauth") || strings.Contains(low, "forbidden") || strings.Contains(low, "token") {
			p.invalidateToken()
			return nil, fmt.Errorf("%w: %s", provider.ErrCredentialsRejected, truncate(text))
		}
		return nil, fmt.Errorf("action: gateway returned errors: %s", truncate(text))
	}
	data := body.Object("data")
	if data == nil {
		return nil, fmt.Errorf("%w: gateway response had no data", provider.ErrEndpointUnavailable)
	}
	return data, nil
}

func errorText(errs []jsonx.Object) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		if m := e.String("message"); m != "" {
			parts = append(parts, m)
		}
	}
	if len(parts) == 0 {
		return "unspecified GraphQL error"
	}
	return strings.Join(parts, "; ")
}

// token returns a bearer token, renewing from the refresh token when needed.
func (p *Provider) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.accessToken != "" && time.Now().Before(p.expiresAt.Add(-30*time.Second)) {
		return p.accessToken, nil
	}
	if refresh := p.store.Field(ID, "refresh_token"); refresh != "" {
		return p.refresh(ctx, refresh)
	}
	if static := p.store.Field(ID, "token"); static != "" {
		p.accessToken = static
		p.expiresAt = time.Now().Add(30 * time.Minute)
		return static, nil
	}
	return "", provider.ErrNotLoggedIn
}

// refresh exchanges a refresh token at the Gigya OIDC token endpoint.
func (p *Provider) refresh(ctx context.Context, refresh string) (string, error) {
	clientID := p.store.Field(ID, "client_id")
	if clientID == "" {
		clientID = defaultClientID
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	}.Encode()

	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	tokenEndpoint := p.tokenEndpoint
	if tokenEndpoint == "" {
		tokenEndpoint = gigyaIssuer + "/token"
	}
	_, err := p.client.PostForm(ctx, tokenEndpoint, form,
		map[string]string{"Accept": "application/json"}, &resp)
	if err != nil {
		var statusErr *httpx.StatusError
		if errors.As(err, &statusErr) && (statusErr.Unauthorized() || statusErr.StatusCode == 400) {
			return "", fmt.Errorf("%w: Gigya rejected the refresh token: %s", provider.ErrCredentialsRejected, statusErr.Error())
		}
		return "", err
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("%w: token endpoint returned no access_token", provider.ErrCredentialsRejected)
	}
	p.accessToken = resp.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(max(resp.ExpiresIn, 60)) * time.Second)
	if resp.RefreshToken != "" && resp.RefreshToken != refresh {
		_ = p.store.Merge(ID, map[string]string{"refresh_token": resp.RefreshToken})
	}
	return p.accessToken, nil
}

func (p *Provider) invalidateToken() {
	p.mu.Lock()
	p.accessToken, p.expiresAt = "", time.Time{}
	p.mu.Unlock()
}

func (p *Provider) hasCredentials() bool {
	return p.store.Field(ID, "refresh_token") != "" || p.store.Field(ID, "token") != ""
}

// listReceipt maps one receiptList entry onto the normalized summary shape.
func listReceipt(obj jsonx.Object) receipt.Receipt {
	price := obj.Object("price")
	total, currency := amountOf(price, "total")
	r := receipt.Receipt{
		Provider:    ID,
		ID:          obj.String("id"),
		Source:      receipt.SourceAPI,
		PurchasedAt: obj.Time("dateTime"),
		Currency:    currency,
		Total:       total,
		Link:        receiptsURL,
	}
	if store := obj.Object("store"); store != nil {
		r.Store = &receipt.Store{Name: firstNonEmpty(store.String("name"), "Action"), Code: store.String("id")}
	} else {
		r.Store = &receipt.Store{Name: "Action"}
	}
	r.Notes = append(r.Notes, "item lines available via receipts_get")
	return r
}

// detailReceipt maps a full receipt (products, VAT, payments).
func detailReceipt(id string, obj jsonx.Object) receipt.Receipt {
	price := obj.Object("price")
	total, currency := amountOf(price, "total")
	r := receipt.Receipt{
		Provider:      ID,
		ID:            id,
		Source:        receipt.SourceAPI,
		PurchasedAt:   obj.Time("dateTime"),
		Currency:      currency,
		Total:         total,
		Number:        obj.String("receiptNumber"),
		ItemsComplete: true,
		Link:          receiptsURL,
	}

	store := obj.Object("store")
	r.Store = &receipt.Store{Name: "Action"}
	if store != nil {
		r.Store.Street = store.String("address")
	}

	for _, line := range mustArray(obj, "products") {
		item := productItem(line, currency)
		if item.Name != "" {
			r.Items = append(r.Items, item)
		}
	}

	if price != nil {
		if d, _ := amountOf(price, "employeeDiscount"); !d.IsZero() {
			r.Discounts = append(r.Discounts, receipt.Discount{Description: "employee discount", Amount: d.Abs()})
		}
		if d, _ := amountOf(price, "otherDiscounts"); !d.IsZero() {
			r.Discounts = append(r.Discounts, receipt.Discount{Description: "other discounts", Amount: d.Abs()})
		}
		if vat := price.Object("vat"); vat != nil {
			for _, per := range mustArray(vat, "perPercentage") {
				spec := per.Object("specification")
				amount, _ := amountOf(spec, "vatAmount")
				net, _ := amountOf(spec, "totalExcludingVat")
				gross, _ := amountOf(spec, "totalIncludingVat")
				r.Taxes = append(r.Taxes, receipt.Tax{
					Rate:   per.String("percentage"),
					Amount: amount,
					Net:    net,
					Gross:  gross,
				})
			}
		}
	}

	for _, pm := range stringArray(obj, "paymentMethods") {
		r.Payments = append(r.Payments, receipt.Payment{Method: pm})
	}
	if member := obj.String("membershipId"); member != "" {
		r.Notes = append(r.Notes, "loyalty member "+member)
	}
	return r
}

func productItem(line jsonx.Object, currency string) receipt.Item {
	qty, ok := line.Float("quantity")
	if !ok || qty <= 0 {
		qty = 1
	}
	total, cur := amountOf(line, "totalPrice")
	if cur == "" {
		total = money.New(total.Minor, currency)
	}
	item := receipt.Item{
		Name:     strings.TrimSpace(line.String("description")),
		Code:     line.String("code"),
		Quantity: qty,
		Total:    total,
		Gross:    total,
	}
	if pr := line.Object("price"); pr != nil {
		regular, _ := amountOf(pr, "regularSalesPrice")
		adjusted, _ := amountOf(pr, "adjustedSalesPrice")
		unit := adjusted
		if unit.IsZero() {
			unit = regular
		}
		item.UnitPrice = unit
		if !regular.IsZero() && !adjusted.IsZero() && regular.Minor > adjusted.Minor {
			perUnit := regular.Sub(adjusted)
			item.Discounts = append(item.Discounts, receipt.Discount{
				Description: "price reduction",
				Amount:      money.New(int64(float64(perUnit.Minor)*qty+0.5), currency),
			})
			item.Gross = money.New(int64(float64(regular.Minor)*qty+0.5), currency)
		}
	}
	return item
}

// amountOf reads a money value under key from obj. Action sends the amount as a
// JSON number (major units) alongside a sibling "currency"; jsonx.Amount copes
// with numbers, strings and nested {amount,currency} shapes.
func amountOf(obj jsonx.Object, key string) (money.Amount, string) {
	if obj == nil {
		return money.Amount{}, ""
	}
	amount, currency := jsonx.Amount(obj, key)
	if currency == "" {
		currency = strings.ToUpper(obj.String("currency", "currencyCode"))
		amount = money.New(amount.Minor, currency)
	}
	return amount, currency
}

func mustArray(obj jsonx.Object, key string) []jsonx.Object {
	if obj == nil {
		return nil
	}
	arr, _ := obj.Array(key)
	return arr
}

func stringArray(obj jsonx.Object, key string) []string {
	value, ok := obj.Get(key)
	if !ok {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s := jsonx.AsString(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func pageSize(limit int) int {
	if limit <= 0 || limit > 50 {
		return 50
	}
	return limit
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
