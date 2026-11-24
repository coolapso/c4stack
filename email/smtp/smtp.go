// Package smtp sends email over SMTP. Development uses it with a local mail catcher such
// as Mailpit, so sign-in works offline; production can use it with any provider's SMTP
// server.
//
// The connection must use STARTTLS, except to a loopback host, so passwords and
// messages never cross a network in clear.
package smtp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/coolapso/c4stack/email"
)

// Config configures the adapter.
type Config struct {
	// URL is the server, smtp://[user:password@]host[:port]. The port defaults to 587.
	URL string
	// From is the sender's address, and FromName its name.
	From     string
	FromName string
	// ReplyTo is where replies go. Zero means From.
	ReplyTo string
}

// SMTP implements email.Sender. It doesn't report delivery, so it can't send what must
// be known to arrive, such as credit expiry warnings.
type SMTP struct {
	cfg      Config
	host     string
	addr     string
	auth     smtp.Auth
	loopback bool
	timeout  time.Duration
}

var _ email.Sender = (*SMTP)(nil)

// New validates cfg and returns the adapter. It does not connect.
func New(cfg Config) (*SMTP, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "smtp" || u.Hostname() == "" {
		return nil, errors.New("smtp: the URL must be smtp://[user:password@]host[:port]")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("smtp: the URL must not have a path, query or fragment")
	}
	if !plainAddress(cfg.From) {
		return nil, errors.New("smtp: From must be a plain email address")
	}
	if cfg.ReplyTo != "" && !plainAddress(cfg.ReplyTo) {
		return nil, errors.New("smtp: ReplyTo must be a plain email address")
	}
	if strings.ContainsAny(cfg.FromName, "\r\n") {
		return nil, errors.New("smtp: FromName must be one line")
	}
	port := u.Port()
	if port == "" {
		port = "587"
	}
	s := &SMTP{
		cfg:      cfg,
		host:     u.Hostname(),
		addr:     net.JoinHostPort(u.Hostname(), port),
		loopback: loopback(u.Hostname()),
		timeout:  30 * time.Second,
	}
	if u.User != nil {
		password, _ := u.User.Password()
		s.auth = smtp.PlainAuth("", u.User.Username(), password, s.host)
	}
	return s, nil
}

func plainAddress(a string) bool {
	parsed, err := mail.ParseAddress(a)
	return err == nil && parsed.Name == "" && parsed.Address == a && !strings.ContainsAny(a, "\r\n<>")
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Send delivers m to the SMTP server and returns the message's Message-ID. Errors never
// carry the recipient or the server's words.
func (s *SMTP) Send(ctx context.Context, m email.Message) (string, error) {
	if !plainAddress(m.To) {
		return "", errors.New("smtp: invalid recipient address")
	}
	if m.Subject == "" || m.Text == "" {
		return "", errors.New("smtp: a message needs a subject and text")
	}
	id, msg, err := s.compose(m)
	if err != nil {
		return "", err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return "", fmt.Errorf("smtp: connect: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return "", errors.New("smtp: the server did not greet")
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("localhost"); err != nil {
		return "", errors.New("smtp: the server refused EHLO")
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return "", fmt.Errorf("smtp: STARTTLS: %w", err)
		}
	} else if !s.loopback {
		return "", errors.New("smtp: the server does not offer STARTTLS, which is required off loopback")
	}
	if s.auth != nil {
		if err := c.Auth(s.auth); err != nil {
			return "", errors.New("smtp: authentication failed")
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return "", errors.New("smtp: the server refused the sender")
	}
	if err := c.Rcpt(m.To); err != nil {
		return "", errors.New("smtp: the server refused the recipient")
	}
	w, err := c.Data()
	if err != nil {
		return "", errors.New("smtp: the server refused the message")
	}
	if _, err := w.Write(msg); err != nil {
		return "", errors.New("smtp: writing the message failed")
	}
	if err := w.Close(); err != nil {
		return "", errors.New("smtp: the server refused the message")
	}
	_ = c.Quit()
	return id, nil
}

// compose writes the message: plain text, or plain text and HTML as alternatives, each
// quoted-printable, with its headers on single lines, so nothing in a subject can add a
// header.
func (s *SMTP) compose(m email.Message) (string, []byte, error) {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	domain := s.cfg.From[strings.LastIndexByte(s.cfg.From, '@')+1:]
	id := "<" + hex.EncodeToString(raw) + "@" + domain + ">"
	from := (&mail.Address{Name: s.cfg.FromName, Address: s.cfg.From}).String()
	subject := strings.Join(strings.Fields(m.Subject), " ")

	var b strings.Builder
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from)
	header("To", m.To)
	if s.cfg.ReplyTo != "" {
		header("Reply-To", s.cfg.ReplyTo)
	}
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", time.Now().UTC().Format(time.RFC1123Z))
	header("Message-ID", id)
	header("MIME-Version", "1.0")
	if m.HTML == "" {
		header("Content-Type", "text/plain; charset=utf-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, m.Text); err != nil {
			return "", nil, err
		}
		return id, []byte(b.String()), nil
	}
	// Text first: mail apps show the last alternative they can.
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	for _, alt := range []struct{ contentType, content string }{
		{"text/plain; charset=utf-8", m.Text},
		{"text/html; charset=utf-8", m.HTML},
	} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {alt.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return "", nil, err
		}
		if err := writeQP(w, alt.content); err != nil {
			return "", nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return "", nil, err
	}
	header("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	b.WriteString(body.String())
	return id, []byte(b.String()), nil
}

// writeQP writes s quoted-printable, with CRLF line ends.
func writeQP(w io.Writer, s string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return err
	}
	return qp.Close()
}
