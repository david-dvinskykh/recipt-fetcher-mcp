// Package action reads digital receipts from a Mijn Action account.
//
// Action publishes no API and no documentation for it: the digital receipts
// ("digitale kassabonnen") live in the Action app and in the Mijn Action
// account, and the app talks to endpoints Action has never described publicly.
// Guessing those URLs here would produce a provider that looks configured and
// silently returns nothing, so this package takes the opposite approach: the
// endpoint is configuration, not a constant.
//
// Point api_base (plus, if needed, login_path and receipts_path) at whatever
// the app actually calls, captured from your own session, and the provider maps
// the answer onto the normalized model - the mapping probes field names rather
// than assuming them. With no api_base stored, the provider reports itself as
// unconfigured and every call falls through to the mailbox fallback.
package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// Paths used when the stored credentials do not override them.
const (
	defaultLoginPath    = "/login"
	defaultReceiptsPath = "/receipts"
	receiptsURL         = "https://www.action.com/nl-nl/mijn-action/"
)

// Provider is the Action digital receipt source.
type Provider struct {
	client *httpx.Client
	store  *secret.Store
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
			{Name: "api_base", Description: "base URL of the Mijn Action receipt endpoint; without it this provider stays off and e-mail is used", Required: true},
			{Name: "email", Description: "Mijn Action account e-mail", Required: false},
			{Name: "password", Description: "Mijn Action account password", Required: false, Secret: true},
			{Name: "token", Description: "bearer token captured from an app session, instead of e-mail and password", Required: false, Secret: true},
			{Name: "cookie", Description: "Cookie header from a logged-in Mijn Action session, instead of a token", Required: false, Secret: true},
			{Name: "login_path", Description: "path appended to api_base to exchange e-mail and password for a token; default " + defaultLoginPath, Required: false},
			{Name: "receipts_path", Description: "path appended to api_base that lists receipts; default " + defaultReceiptsPath, Required: false},
		},
		Notes: []string{
			"Action documents no receipt API; the endpoint has to be supplied as configuration",
			"until it is, Action receipts are reconstructed from e-mail, which usually gives the total but not the item lines",
		},
	}
	status.LoggedIn = p.apiBase() != "" && p.hasCredentials()
	if p.apiBase() == "" && p.hasCredentials() {
		status.Error = "credentials stored but api_base is missing, so the API is not called"
	}
	return status
}

// Login stores credentials and, when an api_base is known, verifies them.
func (p *Provider) Login(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	update := map[string]string{}
	for _, name := range []string{"api_base", "email", "password", "token", "cookie", "login_path", "receipts_path"} {
		if value := strings.TrimSpace(fields[name]); value != "" {
			update[name] = strings.TrimSuffix(value, "/")
		}
	}
	if len(update) == 0 {
		return provider.LoginResult{}, errors.New("action: nothing to store; provide at least api_base plus a token, a cookie, or e-mail and password")
	}
	if err := p.store.Merge(ID, update); err != nil {
		return provider.LoginResult{}, err
	}

	result := provider.LoginResult{Provider: ID, OK: true, Stored: p.store.FieldNames(ID)}
	switch {
	case p.apiBase() == "":
		result.OK = false
		result.Message = "credentials stored, but no api_base: Action receipts will come from e-mail only"
		result.Notes = append(result.Notes, "see README, section \"Action\", for how to capture the endpoint from your own session")
		return result, nil
	case !p.hasCredentials():
		result.OK = false
		result.Message = "api_base stored, but no token, cookie or e-mail/password to authenticate with"
		return result, nil
	}

	receipts, err := p.fetch(ctx)
	if err != nil {
		result.OK = false
		result.Message = fmt.Sprintf("credentials stored, but the endpoint did not answer usefully: %v", err)
		result.Notes = append(result.Notes, "listings will fall back to e-mail until this is fixed")
		return result, nil
	}
	result.Message = fmt.Sprintf("Action endpoint answered with %d receipt(s)", len(receipts))
	return result, nil
}

// Logout implements provider.Provider.
func (p *Provider) Logout(ctx context.Context) error { return p.store.Delete(ID) }

// List implements provider.Provider.
func (p *Provider) List(ctx context.Context, q provider.Query) ([]receipt.Receipt, error) {
	q = q.Normalize()
	raw, err := p.fetch(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]receipt.Receipt, 0, len(raw))
	for _, item := range raw {
		r := toReceipt(item)
		if r.ID == "" || !r.InRange(q.From, q.To) {
			continue
		}
		out = append(out, r)
	}
	receipt.SortByDate(out)
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Get implements provider.Provider. When the receipt list carries no line
// items, a per-receipt path (receipts_path/{id}) is tried before giving up.
func (p *Provider) Get(ctx context.Context, id string) (receipt.Receipt, error) {
	raw, err := p.fetch(ctx)
	if err != nil {
		return receipt.Receipt{}, err
	}
	for _, item := range raw {
		r := toReceipt(item)
		if r.ID != id {
			continue
		}
		if len(r.Items) == 0 {
			if detailed, detailErr := p.fetchOne(ctx, id); detailErr == nil {
				detailed.Notes = append(detailed.Notes, r.Notes...)
				return detailed, nil
			}
		}
		if encoded, err := json.Marshal(item); err == nil {
			r.Raw = encoded
		}
		return r, nil
	}
	return receipt.Receipt{}, fmt.Errorf("action: receipt %q not found", id)
}

// fetch reads the receipt list.
func (p *Provider) fetch(ctx context.Context) ([]jsonx.Object, error) {
	data, err := p.request(ctx, p.path("receipts_path", defaultReceiptsPath))
	if err != nil {
		return nil, err
	}
	payload, err := jsonx.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: receipt list is not a JSON object: %v", provider.ErrEndpointUnavailable, err)
	}
	receipts, found := payload.FindArray("receipts", "transactions", "purchases", "tickets", "items", "data", "content")
	if !found {
		return nil, fmt.Errorf("%w: no receipt array in the response; check receipts_path", provider.ErrEndpointUnavailable)
	}
	return receipts, nil
}

// fetchOne reads a single receipt with its item lines.
func (p *Provider) fetchOne(ctx context.Context, id string) (receipt.Receipt, error) {
	data, err := p.request(ctx, p.path("receipts_path", defaultReceiptsPath)+"/"+id)
	if err != nil {
		return receipt.Receipt{}, err
	}
	payload, err := jsonx.Decode(data)
	if err != nil {
		return receipt.Receipt{}, fmt.Errorf("%w: receipt detail is not a JSON object: %v", provider.ErrEndpointUnavailable, err)
	}
	// Some APIs wrap the detail in {"receipt": {...}} or {"data": {...}}.
	if nested := payload.Object("receipt", "data", "transaction"); nested != nil {
		payload = nested
	}
	r := toReceipt(payload)
	r.ID = id
	r.Raw = json.RawMessage(data)
	return r, nil
}

// request performs an authenticated GET against the configured base.
func (p *Provider) request(ctx context.Context, path string) ([]byte, error) {
	base := p.apiBase()
	if base == "" {
		return nil, fmt.Errorf("%w: no api_base configured for Action", provider.ErrNotSupported)
	}
	if !p.hasCredentials() {
		return nil, provider.ErrNotLoggedIn
	}

	token, err := p.bearer(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie := p.store.Field(ID, "cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	data, resp, err := p.client.Do(ctx, req)
	if err != nil {
		var statusErr *httpx.StatusError
		if errors.As(err, &statusErr) {
			switch {
			case statusErr.Unauthorized():
				// A stored token may simply have aged out; drop it so the next
				// call logs in again with e-mail and password if it can.
				_ = p.store.Merge(ID, map[string]string{"token_cached": ""})
				return nil, fmt.Errorf("%w: %s", provider.ErrCredentialsRejected, statusErr.Error())
			case statusErr.NotFound():
				return nil, fmt.Errorf("%w: %s", provider.ErrEndpointUnavailable, statusErr.Error())
			}
		}
		return nil, err
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "json") {
		return nil, fmt.Errorf("%w: endpoint answered %s instead of JSON", provider.ErrEndpointUnavailable, contentType)
	}
	return data, nil
}

// bearer returns the token to send, logging in with e-mail and password when
// only those are stored.
func (p *Provider) bearer(ctx context.Context) (string, error) {
	if token := p.store.Field(ID, "token"); token != "" {
		return token, nil
	}
	if cached := p.store.Field(ID, "token_cached"); cached != "" {
		return cached, nil
	}
	email, password := p.store.Field(ID, "email"), p.store.Field(ID, "password")
	if email == "" || password == "" {
		// A cookie alone is a valid way to authenticate.
		if p.store.Field(ID, "cookie") != "" {
			return "", nil
		}
		return "", provider.ErrNotLoggedIn
	}

	var payload map[string]any
	_, err := p.client.PostJSON(ctx, p.apiBase()+p.path("login_path", defaultLoginPath),
		map[string]string{"email": email, "username": email, "password": password},
		map[string]string{"Accept": "application/json"}, &payload)
	if err != nil {
		var statusErr *httpx.StatusError
		if errors.As(err, &statusErr) {
			if statusErr.Unauthorized() {
				return "", fmt.Errorf("%w: %s", provider.ErrCredentialsRejected, statusErr.Error())
			}
			if statusErr.NotFound() {
				return "", fmt.Errorf("%w: login endpoint not found, check login_path", provider.ErrEndpointUnavailable)
			}
		}
		return "", err
	}

	obj := jsonx.Object(payload)
	token := obj.String("access_token", "accessToken", "token", "idToken", "jwt")
	if token == "" {
		if nested := obj.Object("data", "result", "session"); nested != nil {
			token = nested.String("access_token", "accessToken", "token", "idToken", "jwt")
		}
	}
	if token == "" {
		return "", fmt.Errorf("%w: login response carried no token", provider.ErrEndpointUnavailable)
	}
	if err := p.store.Merge(ID, map[string]string{"token_cached": token}); err != nil {
		return "", err
	}
	return token, nil
}

func (p *Provider) apiBase() string {
	return strings.TrimSuffix(p.store.Field(ID, "api_base"), "/")
}

func (p *Provider) path(field, fallback string) string {
	value := p.store.Field(ID, field)
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}

func (p *Provider) hasCredentials() bool {
	if p.store.Field(ID, "token") != "" || p.store.Field(ID, "cookie") != "" {
		return true
	}
	return p.store.Field(ID, "email") != "" && p.store.Field(ID, "password") != ""
}

// toReceipt maps one receipt object onto the normalized model, probing for
// field names rather than assuming a documented shape.
func toReceipt(obj jsonx.Object) receipt.Receipt {
	total, currency := jsonx.Amount(obj, "total", "totalAmount", "amount", "grandTotal", "totalPrice", "sum")
	if currency == "" {
		currency = strings.ToUpper(obj.String("currency", "currencyCode"))
		total = money.New(total.Minor, currency)
	}

	r := receipt.Receipt{
		Provider:    ID,
		ID:          obj.String("id", "receiptId", "transactionId", "number", "uuid"),
		Source:      receipt.SourceAPI,
		PurchasedAt: firstTime(obj, "date", "purchaseDate", "transactionDate", "createdAt", "timestamp", "dateTime"),
		Currency:    currency,
		Total:       total,
		Number:      obj.String("receiptNumber", "number", "ticketNumber"),
		Link:        receiptsURL,
	}

	storeName := obj.String("storeName", "shopName", "branch")
	if storeObj := obj.Object("store", "shop", "location"); storeObj != nil {
		r.Store = &receipt.Store{
			Name:    firstNonEmpty(storeObj.String("name", "title", "displayName"), storeName, "Action"),
			Code:    storeObj.String("id", "code", "storeId", "number"),
			Street:  storeObj.String("street", "address", "addressLine1"),
			City:    storeObj.String("city", "town", "locality"),
			Country: storeObj.String("country", "countryCode"),
		}
	} else {
		r.Store = &receipt.Store{Name: firstNonEmpty(storeName, "Action")}
	}

	lines, _ := obj.FindArray("lines", "items", "products", "articles", "lineItems")
	for _, line := range lines {
		item := toItem(line, currency)
		if item.Name == "" {
			continue
		}
		r.Items = append(r.Items, item)
	}
	r.ItemsComplete = len(r.Items) > 0
	return r
}

func toItem(line jsonx.Object, currency string) receipt.Item {
	unitPrice, itemCurrency := jsonx.Amount(line, "unitPrice", "price", "pricePerUnit")
	if itemCurrency == "" {
		itemCurrency = currency
		unitPrice = money.New(unitPrice.Minor, currency)
	}
	quantity, ok := line.Float("quantity", "qty", "count", "amount")
	if !ok || quantity <= 0 {
		quantity = 1
	}

	item := receipt.Item{
		Name:      strings.TrimSpace(line.String("name", "description", "title", "productName", "article")),
		Quantity:  quantity,
		UnitPrice: unitPrice,
		Code:      line.String("code", "ean", "sku", "articleNumber", "barcode"),
		TaxGroup:  line.String("taxGroup", "vatGroup", "taxRate", "vatRate"),
		Category:  line.String("category", "categoryName", "department"),
	}

	total, _ := jsonx.Amount(line, "total", "totalPrice", "lineTotal", "amount")
	if total.IsZero() {
		total = money.New(int64(float64(unitPrice.Minor)*quantity+0.5), itemCurrency)
	} else if total.Currency == "" {
		total = money.New(total.Minor, itemCurrency)
	}
	item.Total = total
	item.Gross = total

	if discount, _ := jsonx.Amount(line, "discount", "discountAmount", "reduction"); !discount.IsZero() {
		amount := discount.Abs()
		item.Discounts = append(item.Discounts, receipt.Discount{Description: line.String("discountDescription", "promotion"), Amount: amount})
		item.Gross = total.Add(amount)
	}
	return item
}

func firstTime(obj jsonx.Object, keys ...string) time.Time {
	for _, key := range keys {
		if t := obj.Time(key); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
