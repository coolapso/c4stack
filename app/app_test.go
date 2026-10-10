package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHelpListsCommandsWithoutDatabase(t *testing.T) {
	cfg := Config{Name: "acme", Setup: func(ctx context.Context, a *App) error {
		if a.DB() != nil {
			t.Error("help opened the database")
		}
		a.Command(Command{Name: "credits adjust", Usage: "<account-id> <cents>", Help: "Adjust credit."})
		return nil
	}}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), cfg, []string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"serve", "migrate status", "credits adjust <account-id> <cents>", "Adjust credit."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, out.String())
		}
	}

	failing := Config{Name: "acme", Setup: func(context.Context, *App) error { return errors.New("ACME_HANKO_API_URL is not set") }}
	out.Reset()
	run(context.Background(), failing, []string{"help"}, &out, &errOut)
	if !strings.Contains(out.String(), "migrate") || !strings.Contains(out.String(), "ACME_HANKO_API_URL is not set") {
		t.Errorf("help with incomplete configuration:\n%s", out.String())
	}
}

func TestFindPrefersLongestName(t *testing.T) {
	a := New(Config{}, nil)
	a.Command(Command{Name: "credits"})
	a.Command(Command{Name: "credits adjust"})
	cmd, rest, ok := a.find([]string{"credits", "adjust", "id", "5"})
	if !ok || cmd.Name != "credits adjust" || len(rest) != 2 {
		t.Fatalf("find: %q %v %v", cmd.Name, rest, ok)
	}
	if _, _, ok := a.find([]string{"accounts"}); ok {
		t.Fatal("found an unregistered command")
	}
}

func TestRequire(t *testing.T) {
	a := New(Config{Migrations: []Migrations{{Namespace: "accounts"}}}, nil)
	if err := a.Require(Migrations{Namespace: "accounts"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Require(Migrations{Namespace: "apikeys"}); err == nil || !strings.Contains(err.Error(), "apikeys") {
		t.Fatalf("missing migrations: %v", err)
	}
}

func TestConfigNeedsSetup(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), Config{Name: "acme"}, []string{"serve"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
}
