package scaleway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/email"
)

func TestNew(t *testing.T) {
	for _, cfg := range []Config{
		{ProjectID: "p", From: "noreply@acme.example"},
		{SecretKey: "s", ProjectID: "p", From: "not an address"},
		{SecretKey: "s", ProjectID: "p", From: "Acme <noreply@acme.example>"},
		{SecretKey: "s", ProjectID: "p", From: "noreply@acme.example", ReplyTo: "nope"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestSend(t *testing.T) {
	var got map[string]any
	var path, token string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.Header.Get("X-Auth-Token")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status != http.StatusOK {
			io.WriteString(w, `{"message":"rejected ada@example.com"}`)
			return
		}
		io.WriteString(w, `{"emails":[{"id":"655c27f2","status":"new","rcpt_to":"ada@example.com"}]}`)
	}))
	defer srv.Close()
	s, err := New(Config{SecretKey: "scw-secret", ProjectID: "proj", From: "noreply@acme.example", FromName: "Acme", ReplyTo: "help@acme.example", APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m := email.Message{To: "ada@example.com", Subject: "Hello", Text: "Body"}
	id, err := s.Send(context.Background(), m)
	if err != nil || id != "655c27f2" {
		t.Fatalf("send: %q %v", id, err)
	}
	if path != "/transactional-email/v1alpha1/regions/fr-par/emails" || token != "scw-secret" {
		t.Errorf("path %q token %q", path, token)
	}
	if got["project_id"] != "proj" || got["subject"] != "Hello" || got["text"] != "Body" ||
		got["to"].([]any)[0].(map[string]any)["email"] != "ada@example.com" ||
		got["from"].(map[string]any)["name"] != "Acme" ||
		got["additional_headers"].([]any)[0].(map[string]any)["value"] != "help@acme.example" {
		t.Errorf("request %v", got)
	}

	status = http.StatusUnprocessableEntity
	_, err = s.Send(context.Background(), m)
	if err == nil || strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("error %v", err)
	}
	if _, err := s.Send(context.Background(), email.Message{To: "Ada <ada@example.com>", Subject: "s", Text: "t"}); err == nil {
		t.Error("accepted a recipient with a display name")
	}
}

func TestDelivery(t *testing.T) {
	var path string
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		io.WriteString(w, body)
	}))
	defer srv.Close()
	s, _ := New(Config{SecretKey: "s", ProjectID: "p", From: "noreply@acme.example", APIURL: srv.URL})
	cases := []struct {
		body string
		want email.Delivery
	}{
		{`{"status":"new"}`, email.Pending},
		{`{"status":"sending","try_count":1}`, email.Pending},
		{`{"status":"sent","updated_at":"2024-04-01T07:55:41.266916Z"}`, email.Delivered},
		{`{"status":"failed"}`, email.Failed},
		{`{"status":"canceled"}`, email.Failed},
	}
	for _, c := range cases {
		body = c.body
		got, at, err := s.Delivery(context.Background(), "655c27f2")
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v", c.body, got, err)
		}
		if got == email.Delivered && !at.Equal(time.Date(2024, 4, 1, 7, 55, 41, 266916000, time.UTC)) {
			t.Errorf("delivered at %v", at)
		}
	}
	if path != "/transactional-email/v1alpha1/regions/fr-par/emails/655c27f2" {
		t.Errorf("path %q", path)
	}
}
