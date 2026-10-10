// Package email defines the transactional email sender interface.
package email

import "context"

// Message is a plain-text email to one recipient.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender delivers transactional email, such as Scaleway Transactional Email.
type Sender interface {
	// Send returns nil only once the provider has accepted the message.
	Send(ctx context.Context, m Message) error
}
