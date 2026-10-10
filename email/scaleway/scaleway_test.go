package scaleway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		io.WriteString(w, `{"message":"rejected ada@example.com"}`)
	}))
	defer srv.Close()
	s, err := New(Config{SecretKey: "scw-secret", ProjectID: "proj", From: "noreply@acme.example", FromName: "Acme", ReplyTo: "help@acme.example", APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	m := email.Message{To: "ada@example.com", Subject: "Hello", Text: "Body"}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
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
	err = s.Send(context.Background(), m)
	if err == nil || strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("error %v", err)
	}
	if err := s.Send(context.Background(), email.Message{To: "Ada <ada@example.com>", Subject: "s", Text: "t"}); err == nil {
		t.Error("accepted a recipient with a display name")
	}
}
