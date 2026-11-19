package smtp

import (
	"bufio"
	"context"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"

	"github.com/coolapso/c4stack/email"
)

// fakeServer is a minimal SMTP server on loopback that records one message per
// connection.
type fakeServer struct {
	addr     string
	starttls bool
	mu       sync.Mutex
	from     string
	to       []string
	data     string
}

func newFakeServer(t *testing.T, starttls bool) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeServer{addr: ln.Addr().String(), starttls: starttls}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	reply := func(line string) { io.WriteString(conn, line+"\r\n") }
	reply("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if s.starttls {
				reply("250-fake")
				reply("250 STARTTLS")
			} else {
				reply("250 fake")
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.mu.Lock()
			s.from = strings.Trim(cmd[len("MAIL FROM:"):], "<> ")
			s.mu.Unlock()
			reply("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			s.mu.Lock()
			s.to = append(s.to, strings.Trim(cmd[len("RCPT TO:"):], "<> "))
			s.mu.Unlock()
			reply("250 ok")
		case upper == "DATA":
			reply("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			reply("250 queued")
		case upper == "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not here")
		}
	}
}

func TestNew(t *testing.T) {
	for _, cfg := range []Config{
		{URL: "", From: "noreply@acme.example"},
		{URL: "http://localhost:1025", From: "noreply@acme.example"},
		{URL: "smtp://localhost:1025/path", From: "noreply@acme.example"},
		{URL: "smtp://localhost:1025", From: "Acme <noreply@acme.example>"},
		{URL: "smtp://localhost:1025", From: "noreply@acme.example", ReplyTo: "nope"},
		{URL: "smtp://localhost:1025", From: "noreply@acme.example", FromName: "Acme\r\nBcc: x@evil.example"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	s, err := New(Config{URL: "smtp://user:pw@mail.acme.example", From: "noreply@acme.example"})
	if err != nil || s.addr != "mail.acme.example:587" || s.auth == nil || s.loopback {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestSend(t *testing.T) {
	srv := newFakeServer(t, false)
	s, err := New(Config{URL: "smtp://" + srv.addr, From: "noreply@acme.example", FromName: "Acme", ReplyTo: "support@acme.example"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Send(context.Background(), email.Message{
		To:      "ann@example.com",
		Subject: "Your Acme sign-in code\r\nBcc: evil@example.com",
		Text:    "Hello,\n\nHere is your code: 4821 0937. Ça marche.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.from != "noreply@acme.example" || len(srv.to) != 1 || srv.to[0] != "ann@example.com" {
		t.Fatalf("envelope: %q %v", srv.from, srv.to)
	}
	msg, err := mail.ReadMessage(strings.NewReader(srv.data))
	if err != nil {
		t.Fatal(err)
	}
	h := msg.Header
	if h.Get("Message-ID") != id || h.Get("Reply-To") != "support@acme.example" || h.Get("Bcc") != "" {
		t.Fatalf("headers: %v", h)
	}
	if subject, _ := new(mime.WordDecoder).DecodeHeader(h.Get("Subject")); subject != "Your Acme sign-in code Bcc: evil@example.com" {
		t.Fatalf("subject %q", subject)
	}
	if from, _ := mail.ParseAddress(h.Get("From")); from == nil || from.Address != "noreply@acme.example" || from.Name != "Acme" {
		t.Fatalf("from %q", h.Get("From"))
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if !strings.Contains(string(body), "4821 0937. Ça marche.") {
		t.Fatalf("body %q", body)
	}
}

func TestSendRefusesBadRecipients(t *testing.T) {
	srv := newFakeServer(t, false)
	s, _ := New(Config{URL: "smtp://" + srv.addr, From: "noreply@acme.example"})
	for _, to := range []string{"", "Ann <ann@example.com>", "ann@example.com\r\nBcc: x@example.com", "a@example.com, b@example.com"} {
		if _, err := s.Send(context.Background(), email.Message{To: to, Subject: "s", Text: "t"}); err == nil {
			t.Errorf("sent to %q", to)
		}
	}
}

func TestSendNeedsSTARTTLSOffLoopback(t *testing.T) {
	srv := newFakeServer(t, false)
	s, _ := New(Config{URL: "smtp://" + srv.addr, From: "noreply@acme.example"})
	s.loopback = false // as if the server were across a network
	_, err := s.Send(context.Background(), email.Message{To: "ann@example.com", Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err %v", err)
	}
}
