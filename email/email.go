// Package email defines the transactional email sender interface.
package email

import (
	"context"
	"time"
)

// Message is a plain-text email to one recipient.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender sends transactional email, such as Scaleway Transactional Email.
type Sender interface {
	// Send hands m to the provider and returns the provider's ID for it. A nil error
	// means the provider accepted the message, not that it arrived.
	Send(ctx context.Context, m Message) (id string, err error)
}

// Delivery is what became of a message the provider accepted.
type Delivery int

const (
	// Pending means the provider is still trying.
	Pending Delivery = iota
	// Delivered means the recipient's mail system took the message.
	Delivered
	// Failed means the recipient's mail system refused the message, or the provider
	// gave up or cancelled it.
	Failed
)

// Tracker reports what became of messages, for providers that can tell.
type Tracker interface {
	// Delivery returns the state of the message with the given ID and, once it was
	// delivered, when.
	Delivery(ctx context.Context, id string) (Delivery, time.Time, error)
}

// TrackingSender sends messages and reports their delivery. Anything that must know a
// message arrived, such as a warning before credit expires, needs one.
type TrackingSender interface {
	Sender
	Tracker
}
