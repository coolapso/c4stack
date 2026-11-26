package creem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coolapso/c4stack/payments"
)

var _ payments.Subscriber = (*Creem)(nil)

// referenceKey is the checkout metadata that carries the app's reference. Creem copies
// a checkout's metadata to its subscription, so every subscription event carries it.
const referenceKey = "reference"

// CreateSubscriptionCheckout creates a checkout for a recurring product. The reference
// travels as the request ID and in the metadata.
func (c *Creem) CreateSubscriptionCheckout(ctx context.Context, co payments.SubscriptionCheckout) (payments.Session, error) {
	if co.ProductID == "" || co.Reference == "" || co.Units < 1 {
		return payments.Session{}, errors.New("creem: a subscription checkout needs a product, a reference and units")
	}
	body := map[string]any{
		"product_id":  co.ProductID,
		"request_id":  co.Reference,
		"units":       co.Units,
		"success_url": co.SuccessURL,
		"metadata":    map[string]string{referenceKey: co.Reference},
	}
	if co.Email != "" {
		body["customer"] = map[string]string{"email": co.Email}
	}
	if co.DiscountCode != "" {
		body["discount_code"] = co.DiscountCode
	}
	var result struct {
		ID  string `json:"id"`
		URL string `json:"checkout_url"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/checkouts", body, &result); err != nil {
		return payments.Session{}, err
	}
	if result.ID == "" {
		return payments.Session{}, errors.New("creem: unreadable checkout response")
	}
	checkoutURL, err := c.checkoutURL(result.URL)
	if err != nil {
		return payments.Session{}, err
	}
	return payments.Session{ID: result.ID, URL: checkoutURL}, nil
}

// CompletedCheckout reads a checkout, and returns the subscription it started.
func (c *Creem) CompletedCheckout(ctx context.Context, id string) (payments.SubscriptionEvent, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodGet, "/v1/checkouts?"+url.Values{"checkout_id": {id}}.Encode(), nil, &raw); err != nil {
		return payments.SubscriptionEvent{}, err
	}
	e, err := checkoutEvent(raw)
	if err != nil {
		return e, err
	}
	if e.Review == "checkout_not_completed" {
		return payments.SubscriptionEvent{}, payments.ErrIgnored
	}
	e.ID = "checkout:" + id
	return e, nil
}

// checkoutEvent reads a checkout object, from a webhook or the API, as the start of a
// subscription. A checkout that bought no subscription is ErrIgnored.
func checkoutEvent(raw json.RawMessage) (payments.SubscriptionEvent, error) {
	var co struct {
		RequestID    string            `json:"request_id"`
		Status       string            `json:"status"`
		Units        int               `json:"units"`
		Metadata     map[string]string `json:"metadata"`
		Order        order             `json:"order"`
		Subscription *subscription     `json:"subscription"`
	}
	if json.Unmarshal(raw, &co) != nil {
		return payments.SubscriptionEvent{}, payments.ErrInvalid
	}
	if co.Order.Type != "recurring" || co.Subscription == nil || co.Subscription.ID == "" {
		if co.Status != "completed" && co.Order.Type == "" {
			// An open checkout has no order yet: whether it buys a subscription shows
			// once it completes.
			return payments.SubscriptionEvent{Review: "checkout_not_completed"}, nil
		}
		return payments.SubscriptionEvent{}, payments.ErrIgnored
	}
	e := payments.SubscriptionEvent{Kind: payments.SubscriptionStarted, Reference: co.RequestID}
	e.State, e.Review = co.Subscription.state()
	if e.State.Units == 0 {
		e.State.Units = co.Units
	}
	switch {
	case co.Status != "completed":
		e.Review = "checkout_not_completed"
	case co.RequestID == "" || co.Metadata[referenceKey] != co.RequestID:
		e.Review = "missing_reference"
	}
	return e, nil
}

// Subscription fetches a subscription.
func (c *Creem) Subscription(ctx context.Context, id string) (payments.SubscriptionState, error) {
	var s subscription
	if err := c.call(ctx, http.MethodGet, "/v1/subscriptions?"+url.Values{"subscription_id": {id}}.Encode(), nil, &s); err != nil {
		return payments.SubscriptionState{}, err
	}
	state, review := s.state()
	if review != "" {
		return state, fmt.Errorf("creem: subscription %s: %s", id, review)
	}
	return state, nil
}

// CancelSubscription cancels a subscription, at the end of its period or at once.
func (c *Creem) CancelSubscription(ctx context.Context, id string, atPeriodEnd bool) error {
	body := map[string]string{"mode": "immediate"}
	if atPeriodEnd {
		body = map[string]string{"mode": "scheduled", "onExecute": "cancel"}
	}
	return c.call(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(id)+"/cancel", body, nil)
}

// ChangeSubscription upgrades a subscription to another product, and changes its units
// on its one item.
func (c *Creem) ChangeSubscription(ctx context.Context, id string, ch payments.SubscriptionChange) error {
	behavior := map[payments.Proration]string{
		payments.ProrateNow:         "proration-charge-immediately",
		payments.ProrateNextInvoice: "proration-charge",
		payments.ProrateNone:        "proration-none",
	}[ch.Proration]
	if behavior == "" {
		return errors.New("creem: unknown proration")
	}
	path := "/v1/subscriptions/" + url.PathEscape(id)
	if ch.ProductID != "" {
		body := map[string]string{"product_id": ch.ProductID, "update_behavior": behavior}
		if err := c.call(ctx, http.MethodPost, path+"/upgrade", body, nil); err != nil {
			return err
		}
	}
	if ch.Units == 0 {
		return nil
	}
	var s subscription
	if err := c.call(ctx, http.MethodGet, "/v1/subscriptions?"+url.Values{"subscription_id": {id}}.Encode(), nil, &s); err != nil {
		return err
	}
	if len(s.Items) != 1 {
		return fmt.Errorf("creem: subscription %s has %d items, not one", id, len(s.Items))
	}
	item := map[string]any{"id": s.Items[0].ID, "units": ch.Units}
	if s.Items[0].PriceID != "" {
		item["price_id"] = s.Items[0].PriceID
	}
	return c.call(ctx, http.MethodPost, path, map[string]any{"items": []any{item}, "update_behavior": behavior}, nil)
}

// CustomerPortal returns a link to the customer's billing portal.
func (c *Creem) CustomerPortal(ctx context.Context, customerID string) (string, error) {
	var result struct {
		Link string `json:"customer_portal_link"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/customers/billing", map[string]string{"customer_id": customerID}, &result); err != nil {
		return "", err
	}
	u, err := url.Parse(result.Link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("creem: unreadable portal link")
	}
	return result.Link, nil
}

// ParseSubscriptionWebhook verifies and decodes a Creem webhook about a subscription:
// a checkout that started one, any subscription event, or a refund or dispute of one.
func (c *Creem) ParseSubscriptionWebhook(header http.Header, body []byte) (payments.SubscriptionEvent, error) {
	secret := c.cfg.SubscriptionWebhookSecret
	if secret == "" {
		secret = c.cfg.WebhookSecret
	}
	env, err := verify(header, body, secret)
	if err != nil {
		return payments.SubscriptionEvent{}, err
	}
	e := payments.SubscriptionEvent{ID: env.ID}
	if env.CreatedAt > 0 {
		e.CreatedAt = time.UnixMilli(env.CreatedAt).UTC()
	}
	switch {
	case env.Type == "checkout.completed":
		started, err := checkoutEvent(env.Object)
		if err != nil {
			return e, err
		}
		if started.Kind == 0 {
			return e, payments.ErrIgnored
		}
		started.ID, started.CreatedAt = e.ID, e.CreatedAt
		e = started
	case len(env.Type) > len("subscription.") && env.Type[:len("subscription.")] == "subscription.":
		var s subscription
		if json.Unmarshal(env.Object, &s) != nil || s.ID == "" {
			return payments.SubscriptionEvent{}, payments.ErrInvalid
		}
		e.Kind = payments.SubscriptionChanged
		e.Reference = s.Metadata[referenceKey]
		e.State, e.Review = s.state()
	case env.Type == "refund.created" || env.Type == "dispute.created":
		var r struct {
			Subscription *subscription `json:"subscription"`
		}
		if json.Unmarshal(env.Object, &r) != nil {
			return payments.SubscriptionEvent{}, payments.ErrInvalid
		}
		if r.Subscription == nil || r.Subscription.ID == "" {
			return e, payments.ErrIgnored
		}
		e.Kind = payments.SubscriptionDisputed
		if env.Type == "refund.created" {
			e.Kind = payments.SubscriptionRefunded
		}
		e.Reference = r.Subscription.Metadata[referenceKey]
		// A refund's or dispute's subscription is a summary, not its whole state.
		e.State = payments.SubscriptionState{ID: r.Subscription.ID}
	default:
		return e, payments.ErrIgnored
	}
	return e, nil
}

// subscription is the part of Creem's subscription object the adapter reads.
type subscription struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Product  ref               `json:"product"`
	Customer ref               `json:"customer"`
	Metadata map[string]string `json:"metadata"`
	// PeriodStart and LastTransaction are read only by quotes.
	PeriodStart     string   `json:"current_period_start_date"`
	PeriodEnd       string   `json:"current_period_end_date"`
	LastTransaction *payment `json:"last_transaction"`
	Items           []struct {
		ID        string `json:"id"`
		ProductID string `json:"product_id"`
		PriceID   string `json:"price_id"`
		Units     int    `json:"units"`
	} `json:"items"`
}

func (s *subscription) UnmarshalJSON(data []byte) error {
	type plain subscription
	return expanded(data, &s.ID, (*plain)(s))
}

// statuses maps Creem's subscription statuses to the module's.
var statuses = map[string]payments.SubscriptionStatus{
	"active":           payments.SubscriptionActive,
	"trialing":         payments.SubscriptionTrialing,
	"past_due":         payments.SubscriptionPastDue,
	"unpaid":           payments.SubscriptionUnpaid,
	"paused":           payments.SubscriptionPaused,
	"scheduled_cancel": payments.SubscriptionCanceling,
	"canceled":         payments.SubscriptionCanceled,
}

// state returns the subscription's state, and why it can't be applied, if it can't.
func (s *subscription) state() (payments.SubscriptionState, string) {
	st := payments.SubscriptionState{
		ID:         s.ID,
		CustomerID: string(s.Customer),
		ProductID:  string(s.Product),
	}
	review := ""
	if s.Status != "" {
		var ok bool
		if st.Status, ok = statuses[s.Status]; !ok {
			review = "unknown_status"
		}
	}
	if s.PeriodEnd != "" {
		if t, err := time.Parse(time.RFC3339, s.PeriodEnd); err == nil {
			st.PeriodEnd = t.UTC()
		}
	}
	switch len(s.Items) {
	case 0:
	case 1:
		// The item is what is billed: after a change, it names the product the
		// subscription has now, which the subscription's own product field may not.
		st.Units = s.Items[0].Units
		if s.Items[0].ProductID != "" {
			st.ProductID = s.Items[0].ProductID
		}
	default:
		review = "several_items"
	}
	return st, review
}

// envelope is a verified webhook's outer object.
type envelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"eventType"`
	CreatedAt int64           `json:"created_at"`
	Object    json.RawMessage `json:"object"`
}

// call sends a request to Creem's API and decodes the answer into out, if out isn't nil.
// Errors carry Creem's status code, never the request or the answer.
func (c *Creem) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Api-Key", c.cfg.APIKey)
	resp, err := c.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("creem: %s %s: %w", method, endpoint(path), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("creem: %s %s answered %d", method, endpoint(path), resp.StatusCode)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return fmt.Errorf("creem: %s %s: unreadable answer", method, endpoint(path))
	}
	return nil
}

// endpoint is a path without its query, for errors: queries carry IDs.
func endpoint(path string) string {
	if i := bytes.IndexByte([]byte(path), '?'); i >= 0 {
		return path[:i]
	}
	return path
}
