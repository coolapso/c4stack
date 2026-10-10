// Package creem is the Creem payment provider adapter. Every checkout uses one Creem
// product with a custom price that the server computes. Webhooks are verified with the
// HMAC-SHA256 signature Creem sends in the creem-signature header.
package creem

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coolapso/c4stack/payments"
)

// DefaultCheckoutOrigin is where Creem hosts checkout pages unless the store uses its
// own domain.
const DefaultCheckoutOrigin = "https://www.creem.io"

// Config configures the adapter.
type Config struct {
	// APIKey is a Creem API key. A key starting with creem_test_ uses Creem's test API.
	APIKey string
	// WebhookSecret verifies webhook signatures.
	WebhookSecret string
	// ProductID is the one-time product every checkout uses, with a custom price.
	ProductID string
	// CheckoutOrigin is where checkout pages are hosted. Zero means
	// DefaultCheckoutOrigin; set it when the store uses a custom domain.
	CheckoutOrigin string
	// APIURL overrides the API base URL. Zero picks Creem's live or test API from the
	// key.
	APIURL string
}

// Creem implements payments.Provider for Creem.
type Creem struct {
	cfg      Config
	api      string
	checkout *url.URL
	client   *http.Client
}

var _ payments.Provider = (*Creem)(nil)

// New validates cfg and returns the adapter. It does not contact Creem.
func New(cfg Config) (*Creem, error) {
	if cfg.APIKey == "" || cfg.WebhookSecret == "" || cfg.ProductID == "" {
		return nil, errors.New("creem: the API key, webhook secret and product ID are required")
	}
	api := cfg.APIURL
	if api == "" {
		api = "https://api.creem.io"
		if strings.HasPrefix(cfg.APIKey, "creem_test_") {
			api = "https://test-api.creem.io"
		}
	}
	if u, err := url.Parse(api); err != nil || u.Host == "" {
		return nil, errors.New("creem: invalid API URL")
	}
	origin := cfg.CheckoutOrigin
	if origin == "" {
		origin = DefaultCheckoutOrigin
	}
	checkout, err := url.Parse(origin)
	if err != nil || checkout.Scheme != "https" || checkout.Host == "" || (checkout.Path != "" && checkout.Path != "/") {
		return nil, errors.New("creem: the checkout origin must be an https origin")
	}
	return &Creem{
		cfg:      cfg,
		api:      strings.TrimSuffix(api, "/"),
		checkout: checkout,
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Name returns "creem".
func (c *Creem) Name() string { return "creem" }

// CheckoutOrigin returns the origin of hosted checkout pages.
func (c *Creem) CheckoutOrigin() string {
	return c.checkout.Scheme + "://" + c.checkout.Host
}

// CreateCheckout creates a checkout for the configured product at the given price.
// The reference travels as the request ID and as metadata, and comes back in webhooks.
func (c *Creem) CreateCheckout(ctx context.Context, co payments.Checkout) (payments.Session, error) {
	body := map[string]any{
		"product_id":   c.cfg.ProductID,
		"request_id":   co.Reference,
		"custom_price": co.AmountCents,
		"units":        1,
		"success_url":  co.SuccessURL,
		"metadata":     map[string]string{"reference": co.Reference},
	}
	if co.Email != "" {
		body["customer"] = map[string]string{"email": co.Email}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+"/v1/checkouts", bytes.NewReader(payload))
	if err != nil {
		return payments.Session{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.cfg.APIKey)
	resp, err := c.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return payments.Session{}, fmt.Errorf("creem: create checkout: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return payments.Session{}, fmt.Errorf("creem: create checkout answered %d", resp.StatusCode)
	}
	var result struct {
		ID  string `json:"id"`
		URL string `json:"checkout_url"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.ID == "" {
		return payments.Session{}, errors.New("creem: unreadable checkout response")
	}
	u, err := url.Parse(result.URL)
	if err != nil || u.Scheme != c.checkout.Scheme || u.Host != c.checkout.Host || u.User != nil {
		return payments.Session{}, errors.New("creem: the checkout URL is not on the checkout origin")
	}
	return payments.Session{ID: result.ID, URL: result.URL}, nil
}

// ParseWebhook verifies and decodes a Creem webhook.
func (c *Creem) ParseWebhook(header http.Header, body []byte) (payments.Event, error) {
	signature, err := hex.DecodeString(header.Get("creem-signature"))
	if err != nil || len(signature) != sha256.Size {
		return payments.Event{}, payments.ErrInvalid
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.WebhookSecret))
	mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return payments.Event{}, payments.ErrInvalid
	}
	var envelope struct {
		ID     string          `json:"id"`
		Type   string          `json:"eventType"`
		Object json.RawMessage `json:"object"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.ID == "" || len(envelope.Object) == 0 {
		return payments.Event{}, payments.ErrInvalid
	}
	switch envelope.Type {
	case "checkout.completed":
		return c.paid(envelope.ID, envelope.Object)
	case "refund.created", "dispute.created":
		return c.reversal(envelope.ID, envelope.Type, envelope.Object)
	}
	return payments.Event{ID: envelope.ID}, payments.ErrIgnored
}

// checkout is the part of Creem's checkout object the adapter reads.
type checkout struct {
	ID        string            `json:"id"`
	RequestID string            `json:"request_id"`
	Status    string            `json:"status"`
	Metadata  map[string]string `json:"metadata"`
	Product   ref               `json:"product"`
	Order     order             `json:"order"`
}

type order struct {
	ID         string `json:"id"`
	Amount     int64  `json:"amount"`
	AmountPaid *int64 `json:"amount_paid"`
	Currency   string `json:"currency"`
	Status     string `json:"status"`
	Type       string `json:"type"`
}

// Creem sends related objects either expanded or as an ID.
func (o *checkout) UnmarshalJSON(data []byte) error {
	type plain checkout
	return expanded(data, &o.ID, (*plain)(o))
}

func (o *order) UnmarshalJSON(data []byte) error {
	type plain order
	return expanded(data, &o.ID, (*plain)(o))
}

// ref is a related object of which only the ID matters.
type ref string

func (r *ref) UnmarshalJSON(data []byte) error {
	var v struct {
		ID string `json:"id"`
	}
	var id string
	err := expanded(data, &id, &v)
	*r = ref(id + v.ID)
	return err
}

func expanded(data []byte, id *string, object any) error {
	if json.Unmarshal(data, id) == nil {
		return nil
	}
	return json.Unmarshal(data, object)
}

func (c *Creem) paid(id string, raw json.RawMessage) (payments.Event, error) {
	var co checkout
	if json.Unmarshal(raw, &co) != nil {
		return payments.Event{}, payments.ErrInvalid
	}
	if co.Order.Type != "onetime" || string(co.Product) != c.cfg.ProductID {
		return payments.Event{ID: id}, payments.ErrIgnored
	}
	e := payments.Event{
		ID:          id,
		Kind:        payments.Paid,
		Reference:   co.RequestID,
		CheckoutID:  co.ID,
		OrderID:     co.Order.ID,
		AmountCents: co.Order.Amount,
		PaidCents:   co.Order.Amount,
		Currency:    strings.ToUpper(co.Order.Currency),
	}
	if co.Order.AmountPaid != nil {
		e.PaidCents = *co.Order.AmountPaid
	}
	switch {
	case co.Status != "completed" || co.Order.Status != "paid":
		e.Review = "checkout_not_paid"
	case co.RequestID == "" || co.Metadata["reference"] != co.RequestID:
		e.Review = "missing_reference"
	}
	return e, nil
}

func (c *Creem) reversal(id, eventType string, raw json.RawMessage) (payments.Event, error) {
	var r struct {
		Amount         int64    `json:"amount"`
		Currency       string   `json:"currency"`
		RefundAmount   int64    `json:"refund_amount"`
		RefundCurrency string   `json:"refund_currency"`
		Status         string   `json:"status"`
		Checkout       checkout `json:"checkout"`
		Order          order    `json:"order"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return payments.Event{}, payments.ErrInvalid
	}
	if r.Checkout.Product != "" && string(r.Checkout.Product) != c.cfg.ProductID {
		return payments.Event{ID: id}, payments.ErrIgnored
	}
	e := payments.Event{
		ID:          id,
		Kind:        payments.Disputed,
		Reference:   r.Checkout.RequestID,
		CheckoutID:  r.Checkout.ID,
		OrderID:     r.Order.ID,
		AmountCents: r.Amount,
		Currency:    strings.ToUpper(r.Currency),
	}
	if e.OrderID == "" {
		e.OrderID = r.Checkout.Order.ID
	}
	if eventType == "refund.created" {
		e.Kind = payments.Refunded
		e.AmountCents = r.RefundAmount
		e.Currency = strings.ToUpper(r.RefundCurrency)
		if r.Status != "" && r.Status != "succeeded" {
			e.Review = "refund_not_succeeded"
		}
	}
	return e, nil
}
