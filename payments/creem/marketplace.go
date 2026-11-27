package creem

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coolapso/c4stack/payments"
)

var _ payments.Marketplace = (*Creem)(nil)

// grantEvent is the event type of Creem's Agentic Marketplace grant requests.
const grantEvent = "marketplace.grant.requested"

// grantObject is the object of a marketplace.grant.requested event. Its fields are
// camelCase, unlike the rest of Creem's webhooks.
type grantObject struct {
	GrantID     string  `json:"grantId"`
	OperationID string  `json:"operationId"`
	Action      string  `json:"action"`
	DeadlineAt  *string `json:"deadlineAt"`
	ListingID   string  `json:"listingId"`
	ProductID   string  `json:"productId"`
	Order       *struct {
		ID          string      `json:"id"`
		Status      string      `json:"status"`
		AmountMinor json.Number `json:"amountMinor"`
		Currency    string      `json:"currency"`
		PaidAt      string      `json:"paidAt"`
	} `json:"order"`
	Test   bool   `json:"test"`
	Reason string `json:"reason"`
}

// ParseGrantRequest verifies and decodes a request from Creem's Agentic Marketplace. It
// arrives at the store's webhook, verified with WebhookSecret; any other event there is
// ErrIgnored.
func (c *Creem) ParseGrantRequest(header http.Header, body []byte) (payments.GrantRequest, error) {
	env, err := verify(header, body, c.cfg.WebhookSecret)
	if err != nil {
		return payments.GrantRequest{}, err
	}
	if env.Type != grantEvent {
		return payments.GrantRequest{EventID: env.ID}, payments.ErrIgnored
	}
	var o grantObject
	if json.Unmarshal(env.Object, &o) != nil || o.GrantID == "" || o.OperationID == "" {
		return payments.GrantRequest{}, payments.ErrInvalid
	}
	r := payments.GrantRequest{
		EventID:     env.ID,
		GrantID:     o.GrantID,
		OperationID: o.OperationID,
		Action:      payments.GrantAction(o.Action),
		ProductID:   o.ProductID,
		ListingID:   o.ListingID,
		Test:        o.Test,
		Reason:      o.Reason,
	}
	switch r.Action {
	case payments.GrantProvision, payments.GrantTopUp, payments.GrantRevoke, payments.GrantRevokeCredit:
	default:
		return payments.GrantRequest{}, payments.ErrInvalid
	}
	if env.CreatedAt > 0 {
		r.CreatedAt = time.UnixMilli(env.CreatedAt).UTC()
	}
	if o.DeadlineAt != nil && *o.DeadlineAt != "" {
		if r.Deadline, err = time.Parse(time.RFC3339Nano, *o.DeadlineAt); err != nil {
			return payments.GrantRequest{}, payments.ErrInvalid
		}
	}
	if o.Order != nil {
		r.OrderID = o.Order.ID
		r.OrderPaid = o.Order.Status == "paid"
		if r.AmountCents, err = minorUnits(o.Order.AmountMinor); err != nil {
			return payments.GrantRequest{}, payments.ErrInvalid
		}
		r.Currency = strings.ToUpper(o.Order.Currency)
		if o.Order.PaidAt != "" {
			if r.PaidAt, err = time.Parse(time.RFC3339Nano, o.Order.PaidAt); err != nil {
				return payments.GrantRequest{}, payments.ErrInvalid
			}
		}
	}
	// A paid request names its order; a test request may name none.
	if r.OrderID == "" && !r.Test && r.Action != payments.GrantRevoke {
		return payments.GrantRequest{}, payments.ErrInvalid
	}
	return r, nil
}

// minorUnits reads an amount in minor units, such as cents. Creem's documentation gives
// it as a JSON number; a whole number written with a fraction, such as 2000.0, counts.
func minorUnits(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil || f != float64(int64(f)) {
		return 0, errors.New("creem: the amount is not a whole number of minor units")
	}
	return int64(f), nil
}

// The limits Creem sets on a grant answer.
const (
	maxEnv            = 16
	maxEnvValue       = 4096
	maxKeyHint        = 8
	maxAccountRef     = 128
	maxEntitlement    = 4096
	maxFailureReason  = 200
	envNamePatternSrc = `^[A-Z][A-Z0-9_]{0,63}$`
)

var envNamePattern = regexp.MustCompile(envNamePatternSrc)

// grantReply is the reply Creem reads. Its fields are snake_case, and Creem refuses
// fields it doesn't know.
type grantReply struct {
	Status        string            `json:"status"`
	Env           map[string]string `json:"env,omitempty"`
	KeyHint       string            `json:"key_hint,omitempty"`
	AccountRef    string            `json:"account_ref,omitempty"`
	Entitlement   json.RawMessage   `json:"entitlement,omitempty"`
	FailureReason string            `json:"failure_reason,omitempty"`
}

// GrantReply encodes an answer as Creem reads it, and refuses one Creem would refuse.
func (c *Creem) GrantReply(a payments.GrantAnswer) ([]byte, error) {
	reply := grantReply{
		Status:        string(a.Status),
		Env:           a.Env,
		KeyHint:       a.KeyHint,
		AccountRef:    a.AccountRef,
		FailureReason: a.FailureReason,
	}
	switch a.Status {
	case payments.GrantReady:
		if a.FailureReason != "" {
			return nil, errors.New("creem: a ready answer has no failure reason")
		}
	case payments.GrantPending:
		if len(a.Env) > 0 || a.FailureReason != "" || a.KeyHint != "" || a.AccountRef != "" || a.Entitlement != nil {
			return nil, errors.New("creem: a pending answer has only its status")
		}
	case payments.GrantFailed:
		if a.FailureReason == "" || utf8.RuneCountInString(a.FailureReason) > maxFailureReason {
			return nil, fmt.Errorf("creem: a failed answer needs a reason of 1 to %d characters", maxFailureReason)
		}
		if len(a.Env) > 0 {
			return nil, errors.New("creem: a failed answer has no credentials")
		}
	default:
		return nil, fmt.Errorf("creem: unknown grant status %q", a.Status)
	}
	if len(a.Env) > maxEnv {
		return nil, fmt.Errorf("creem: at most %d credentials", maxEnv)
	}
	for name, value := range a.Env {
		if !envNamePattern.MatchString(name) {
			return nil, fmt.Errorf("creem: %q is not a credential name Creem accepts", name)
		}
		if value == "" || len(value) > maxEnvValue || !printableASCII(value) {
			return nil, fmt.Errorf("creem: credential %s needs 1 to %d printable ASCII characters", name, maxEnvValue)
		}
	}
	if len(a.KeyHint) > maxKeyHint || len(a.AccountRef) > maxAccountRef {
		return nil, fmt.Errorf("creem: a key hint has at most %d characters, an account reference %d", maxKeyHint, maxAccountRef)
	}
	if a.Entitlement != nil {
		raw, err := json.Marshal(a.Entitlement)
		if err != nil {
			return nil, fmt.Errorf("creem: entitlement: %w", err)
		}
		if len(raw) > maxEntitlement {
			return nil, fmt.Errorf("creem: the entitlement takes at most %d bytes", maxEntitlement)
		}
		reply.Entitlement = raw
	}
	return json.Marshal(reply)
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
