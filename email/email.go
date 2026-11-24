// Package email defines the transactional email sender interface.
package email

import (
	"context"
	"time"
)

// Message is an email to one recipient: plain text, and the same in HTML if it has any.
type Message struct {
	To      string
	Subject string
	// Text is the message in plain text. It is required: it is what mail apps that
	// show no HTML, and people who prefer text, read.
	Text string
	// HTML is the same message as a whole HTML document, for the mail apps that show
	// it. Zero sends the text alone.
	HTML string
}

// Sender sends transactional email, such as Scaleway Transactional Email.
type Sender interface {
	// Send hands m to the provider and returns the provider's ID for it. A nil error
	// means the provider accepted the message, not that it arrived. Callers log the
	// error, so it must not carry the recipient or the message's text.
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
