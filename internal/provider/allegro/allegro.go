// Package allegro reads a buyer's purchase history from Allegro.
//
// Allegro's public REST API (api.allegro.pl, OAuth) covers the seller side:
// GET /order/checkout-forms returns the orders placed *with* you, not the ones
// you placed. The buyer's own "Moje zakupy" list is served by an internal
// endpoint that authenticates with the browser session cookie rather than with
// an OAuth token, which is why this provider asks for a cookie instead of
// running an OAuth flow. See the Allegro developer discussion linked in
// README.md.
//
// A session cookie expires. When it does, the provider reports
// ErrCredentialsRejected and the mailbox fallback takes over.
package allegro

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
const ID = "allegro"

// defaultEndpoint is the buyer order list the Allegro web app itself calls.
const defaultEndpoint = "https://api.allegro.pl/myorder-api/myorders"

// purchasesURL is where a human can look the order up.
const purchasesURL = "https://allegro.pl/moje-allegro/zakupy/kupione"

// acceptHeader is the versioned vendor media type myorder-api expects; a plain
// application/json is answered with a login redirect instead.
const acceptHeader = "application/vnd.allegro.public.v3+json"

// pageSize is how many orders one request asks for.
const pageSize = 50

// Provider is the Allegro buyer purchase source.
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
func (p *Provider) DisplayName() string { return "Allegro" }

// Status implements provider.Provider.
func (p *Provider) Status(ctx context.Context) provider.Status {
	return provider.Status{
		Provider:     ID,
		DisplayName:  p.DisplayName(),
		LoggedIn:     p.store.Field(ID, "cookie") != "",
		StoredFields: p.store.FieldNames(ID),
		Sources:      []string{"api"},
		RequiredFields: []provider.Field{
			{Name: "cookie", Description: "Cookie header from a logged-in allegro.pl session (the QXLSESSID session cookie is the one that matters; see README)", Required: true, Secret: true},
			{Name: "endpoint", Description: "override for the buyer order endpoint; defaults to " + defaultEndpoint, Required: false},
		},
		Notes: []string{
			"Allegro's public OAuth API only exposes seller orders, so the buyer list is read with a browser session cookie",
			"the cookie expires (typically within days); when it does, receipts come from e-mail until you log in again",
		},
	}
}

// Login stores the session cookie and verifies it with one request.
func (p *Provider) Login(ctx context.Context, fields map[string]string) (provider.LoginResult, error) {
	cookie := strings.TrimSpace(fields["cookie"])
	if cookie == "" {
		return provider.LoginResult{}, errors.New("allegro: cookie is required; copy the Cookie header of a logged-in allegro.pl request (see README)")
	}
	update := map[string]string{"cookie": cookie}
	if endpoint := strings.TrimSpace(fields["endpoint"]); endpoint != "" {
		update["endpoint"] = endpoint
	}
	if err := p.store.Merge(ID, update); err != nil {
		return provider.LoginResult{}, err
	}

	result := provider.LoginResult{Provider: ID, OK: true, Stored: p.store.FieldNames(ID)}
	orders, err := p.fetch(ctx, 1)
	switch {
	case err != nil:
		result.OK = false
		result.Message = fmt.Sprintf("cookie stored, but the order endpoint rejected it: %v", err)
		result.Notes = append(result.Notes, "listings will fall back to e-mail until a fresh cookie is stored")
	default:
		result.Message = fmt.Sprintf("Allegro session works (%d order(s) visible on the first page)", len(orders))
	}
	return result, nil
}

// Logout implements provider.Provider.
func (p *Provider) Logout(ctx context.Context) error { return p.store.Delete(ID) }

// List implements provider.Provider.
func (p *Provider) List(ctx context.Context, q provider.Query) ([]receipt.Receipt, error) {
	q = q.Normalize()
	var out []receipt.Receipt

	for offset := 0; offset < 10*pageSize; offset += pageSize {
		orders, err := p.fetch(ctx, offset/pageSize+1)
		if err != nil {
			return nil, err
		}
		if len(orders) == 0 {
			break
		}
		reachedOlder := false
		for _, order := range orders {
			r := toReceipt(order)
			if r.ID == "" {
				continue
			}
			if !q.From.IsZero() && !r.PurchasedAt.IsZero() && r.PurchasedAt.Before(q.From) {
				reachedOlder = true
				continue
			}
			if !q.To.IsZero() && !r.PurchasedAt.IsZero() && r.PurchasedAt.After(q.To) {
				continue
			}
			out = append(out, r)
			if len(out) >= q.Limit {
				return out, nil
			}
		}
		if reachedOlder || len(orders) < pageSize {
			break
		}
	}
	receipt.SortByDate(out)
	return out, nil
}

// Get implements provider.Provider. Allegro exposes no per-order endpoint to a
// buyer, so the order is looked up in the recent pages.
func (p *Provider) Get(ctx context.Context, id string) (receipt.Receipt, error) {
	for offset := 0; offset < 10*pageSize; offset += pageSize {
		orders, err := p.fetch(ctx, offset/pageSize+1)
		if err != nil {
			return receipt.Receipt{}, err
		}
		if len(orders) == 0 {
			break
		}
		for _, order := range orders {
			r := toReceipt(order)
			if r.ID == id {
				if raw, err := json.Marshal(order); err == nil {
					r.Raw = raw
				}
				return r, nil
			}
		}
		if len(orders) < pageSize {
			break
		}
	}
	return receipt.Receipt{}, fmt.Errorf("allegro: order %q not found in the recent purchase pages", id)
}

// fetch reads one page of the buyer's orders.
func (p *Provider) fetch(ctx context.Context, page int) ([]jsonx.Object, error) {
	cookie := p.store.Field(ID, "cookie")
	if cookie == "" {
		return nil, provider.ErrNotLoggedIn
	}
	endpoint := p.store.Field(ID, "endpoint")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	url := fmt.Sprintf("%s?limit=%d&offset=%d", endpoint, pageSize, (page-1)*pageSize)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cookie)
	// myorder-api answers with the buyer order shape only for the versioned
	// vendor Accept header the site itself sends; a plain application/json can
	// get a redirect to login instead. The Referer is part of that same
	// same-origin expectation. (Confirmed against the community Allegro
	// clients Przemko92/home-assistant-allegro and wini83/ff-iii-toolkit-api.)
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("Referer", "https://allegro.pl/")
	req.Header.Set("Accept-Language", "pl-PL,pl;q=0.9")

	data, resp, err := p.client.Do(ctx, req)
	if err != nil {
		var statusErr *httpx.StatusError
		if errors.As(err, &statusErr) {
			switch {
			case statusErr.Unauthorized():
				return nil, fmt.Errorf("%w: allegro session cookie is no longer valid", provider.ErrCredentialsRejected)
			case statusErr.NotFound():
				return nil, fmt.Errorf("%w: %s", provider.ErrEndpointUnavailable, statusErr.Error())
			}
		}
		return nil, err
	}

	// A login redirect answers 200 with HTML, which decodes to nothing useful.
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "json") {
		return nil, fmt.Errorf("%w: order endpoint answered %s instead of JSON, the session is probably not accepted",
			provider.ErrCredentialsRejected, contentType)
	}

	payload, err := jsonx.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot decode the order list: %v", provider.ErrEndpointUnavailable, err)
	}
	orders, found := payload.FindArray("myorders", "orders", "elements", "items", "content", "data")
	if !found {
		return nil, fmt.Errorf("%w: no order array in the response; the endpoint shape changed", provider.ErrEndpointUnavailable)
	}
	return orders, nil
}

// toReceipt maps one order object onto the normalized model. Field names are
// probed rather than assumed, see package jsonx.
func toReceipt(order jsonx.Object) receipt.Receipt {
	total, currency := jsonx.Amount(order, "totalCost", "totalToPay", "total", "amount", "price", "cost")
	r := receipt.Receipt{
		Provider:    ID,
		ID:          order.String("id", "orderId", "checkoutFormId", "groupId", "uuid"),
		Source:      receipt.SourceAPI,
		PurchasedAt: firstTime(order, "boughtAt", "purchaseDate", "orderDate", "createdAt", "date", "occurredAt"),
		Currency:    currency,
		Total:       total,
		Number:      order.String("orderNumber", "number", "id"),
		Link:        purchasesURL,
	}

	seller := sellerName(order)
	if seller != "" {
		r.Store = &receipt.Store{Name: seller}
	} else {
		r.Store = &receipt.Store{Name: "Allegro"}
	}

	offers, _ := order.FindArray("offers", "lineItems", "items", "products")
	for _, offer := range offers {
		item := toItem(offer, currency)
		if item.Name == "" {
			continue
		}
		if item.Seller == "" {
			item.Seller = seller
		}
		r.Items = append(r.Items, item)
	}
	r.ItemsComplete = len(r.Items) > 0

	if r.Total.IsZero() && len(r.Items) > 0 {
		sum := money.New(0, currency)
		for _, item := range r.Items {
			sum = sum.Add(item.Total)
		}
		r.Total = sum
		r.Notes = append(r.Notes, "order total was not in the response; summed from the item lines (delivery excluded)")
	}
	return r
}

func toItem(offer jsonx.Object, currency string) receipt.Item {
	price, itemCurrency := jsonx.Amount(offer, "price", "unitPrice", "cost", "amount", "totalPrice")
	if itemCurrency == "" {
		itemCurrency = currency
	}
	quantity, ok := offer.Float("quantity", "amount", "count")
	if !ok || quantity <= 0 {
		quantity = 1
	}

	item := receipt.Item{
		Name:      strings.TrimSpace(offer.String("name", "title", "offerTitle", "productName")),
		Quantity:  quantity,
		UnitPrice: price,
		Code:      offer.String("id", "offerId", "externalId", "ean"),
		Category:  categoryName(offer),
		Seller:    sellerName(offer),
	}

	// Some payloads price a line per unit, others per line. When the object
	// carries both, the explicit line total wins.
	if lineTotal, lineCurrency := jsonx.Amount(offer, "totalPrice", "lineTotal", "totalCost", "summaryPrice"); !lineTotal.IsZero() {
		if lineCurrency == "" {
			lineTotal = money.New(lineTotal.Minor, itemCurrency)
		}
		item.Total = lineTotal
		if item.UnitPrice.IsZero() && quantity > 0 {
			item.UnitPrice = money.New(int64(float64(lineTotal.Minor)/quantity), itemCurrency)
		}
	} else {
		item.Total = money.New(int64(float64(price.Minor)*quantity+0.5), itemCurrency)
	}
	item.Gross = item.Total
	return item
}

func sellerName(obj jsonx.Object) string {
	if seller := obj.Object("seller", "merchant", "shop"); seller != nil {
		if name := seller.String("login", "name", "companyName", "displayName"); name != "" {
			return name
		}
	}
	return obj.String("sellerLogin", "sellerName")
}

func categoryName(obj jsonx.Object) string {
	if category := obj.Object("category", "productCategory"); category != nil {
		if name := category.String("name", "title", "path"); name != "" {
			return name
		}
	}
	return obj.String("categoryName")
}

func firstTime(obj jsonx.Object, keys ...string) time.Time {
	for _, key := range keys {
		if t := obj.Time(key); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
