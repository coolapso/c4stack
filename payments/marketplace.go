package payments

import (
	"net/http"
	"time"
)

// Marketplace is the optional interface of a provider with a marketplace where AI agents
// buy the app's products and receive their access from the app, such as Creem's Agentic
// Marketplace. The provider asks the app, in a webhook, to grant or revoke what an order
// bought, and takes the app's answer from the webhook's reply.
type Marketplace interface {
	Provider
	// ParseGrantRequest verifies a webhook and decodes a grant request. It returns
	// ErrInvalid for a bad signature or body, and ErrIgnored for any other event, which
	// the app hands to its other webhook handling.
	ParseGrantRequest(header http.Header, body []byte) (GrantRequest, error)
	// GrantReply encodes an answer as the webhook's reply body. It returns an error for
	// an answer the provider would refuse, such as a provision without credentials.
	GrantReply(a GrantAnswer) ([]byte, error)
}

// GrantAction is what a grant request asks for.
type GrantAction string

const (
	// GrantProvision asks for an account and its credentials, for the order.
	GrantProvision GrantAction = "provision"
	// GrantTopUp asks to add what another order bought to the grant's account.
	GrantTopUp GrantAction = "top_up"
	// GrantRevoke reports that the provision's order was refunded or charged back in
	// full: the grant's access ends.
	GrantRevoke GrantAction = "revoke"
	// GrantRevokeCredit reports that a top-up's order was refunded or charged back: what
	// it added is taken back.
	GrantRevokeCredit GrantAction = "revoke_credit"
)

// GrantRequest is a verified request to grant or revoke what a marketplace order
// bought. A grant is one buyer's access, such as an agent's account; its operations
// are the requests about it. The provider may send one request more than once, and
// expects the same answer each time.
type GrantRequest struct {
	// EventID is the provider's event ID.
	EventID string
	// GrantID identifies the grant, the same in every request about it.
	GrantID string
	// OperationID identifies this request, the same each time it is sent.
	OperationID string
	Action      GrantAction
	// ProductID is the provider's product the order is for, and ListingID its listing.
	ProductID string
	ListingID string
	// OrderID is the order the request is about: the one that bought it for a provision
	// or top-up, and the one refunded or charged back for a revocation.
	OrderID string
	// OrderPaid holds when the order is paid.
	OrderPaid bool
	// AmountCents and Currency are what the order came to, as the provider reports it:
	// for Creem, the total, tax included.
	AmountCents int64
	Currency    string
	// PaidAt is when the order was paid, if the request says.
	PaidAt time.Time
	// Deadline is when the provider stops waiting for an answer; zero means it doesn't
	// say. A provision or top-up still unanswered then is refunded to the buyer.
	Deadline time.Time
	// Test marks a request the provider sends while it reviews a listing, about no
	// paid order.
	Test bool
	// Reason says why a revocation came: "refund", "chargeback", "undelivered" or
	// "test", if the request says.
	Reason string
	// CreatedAt is when the provider created the request, if it says.
	CreatedAt time.Time
}

// GrantStatus is how the app answers a grant request.
type GrantStatus string

const (
	// GrantReady means the request is done.
	GrantReady GrantStatus = "ready"
	// GrantPending means the app couldn't finish it now: the provider asks again until
	// the deadline.
	GrantPending GrantStatus = "pending"
	// GrantFailed means the app won't grant it: the provider refunds the buyer.
	GrantFailed GrantStatus = "failed"
)

// GrantAnswer answers a grant request.
type GrantAnswer struct {
	Status GrantStatus
	// Env holds a ready provision's credentials, by the names the listing declares,
	// such as an API key. The provider keeps them for the buyer and shows them once.
	Env map[string]string
	// KeyHint is a short, non-secret part of a key, such as its last characters, that
	// the buyer can name it by.
	KeyHint string
	// AccountRef is the app's reference for the account, if it gives one.
	AccountRef string
	// Entitlement tells the buyer what the order gives them, as a JSON object, such as
	// how much credit and for how long.
	Entitlement map[string]any
	// FailureReason says why a failed request was refused.
	FailureReason string
}
