// Package scaleway sends email through Scaleway Transactional Email.
package scaleway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/coolapso/c4stack/email"
)

// Config configures the adapter.
type Config struct {
	// SecretKey is a Scaleway API secret key allowed to send email.
	SecretKey string
	// ProjectID is the Scaleway project with the verified sending domain.
	ProjectID string
	// Region is where the project's email service runs. Zero means "fr-par".
	Region string
	// From is the sender's address on the verified domain, and FromName its name.
	From     string
	FromName string
	// ReplyTo is where replies go. Zero means From.
	ReplyTo string
	// APIURL overrides Scaleway's API base URL.
	APIURL string
}

// Scaleway implements email.Sender.
type Scaleway struct {
	cfg      Config
	endpoint string
	client   *http.Client
}

var _ email.Sender = (*Scaleway)(nil)

// New validates cfg and returns the adapter. It does not contact Scaleway.
func New(cfg Config) (*Scaleway, error) {
	if cfg.SecretKey == "" || cfg.ProjectID == "" {
		return nil, errors.New("scaleway: the secret key and project ID are required")
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil || strings.Contains(cfg.From, "<") {
		return nil, errors.New("scaleway: From must be a plain email address")
	}
	if cfg.ReplyTo != "" {
		if _, err := mail.ParseAddress(cfg.ReplyTo); err != nil {
			return nil, errors.New("scaleway: ReplyTo must be an email address")
		}
	}
	if cfg.Region == "" {
		cfg.Region = "fr-par"
	}
	api := cfg.APIURL
	if api == "" {
		api = "https://api.scaleway.com"
	}
	return &Scaleway{
		cfg:      cfg,
		endpoint: strings.TrimSuffix(api, "/") + "/transactional-email/v1alpha1/regions/" + url.PathEscape(cfg.Region) + "/emails",
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

type address struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// Send delivers m. Errors carry the HTTP status at most, never the recipient or
// Scaleway's response body.
func (s *Scaleway) Send(ctx context.Context, m email.Message) error {
	if _, err := mail.ParseAddress(m.To); err != nil || strings.Contains(m.To, "<") {
		return errors.New("scaleway: invalid recipient address")
	}
	if m.Subject == "" || m.Text == "" {
		return errors.New("scaleway: a message needs a subject and text")
	}
	body := map[string]any{
		"project_id": s.cfg.ProjectID,
		"from":       address{Email: s.cfg.From, Name: s.cfg.FromName},
		"to":         []address{{Email: m.To}},
		"subject":    m.Subject,
		"text":       m.Text,
	}
	if s.cfg.ReplyTo != "" {
		body["additional_headers"] = []map[string]string{{"key": "Reply-To", "value": s.cfg.ReplyTo}}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", s.cfg.SecretKey)
	resp, err := s.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("scaleway: send: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("scaleway: send answered %d", resp.StatusCode)
	}
	return nil
}
