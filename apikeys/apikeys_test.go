package apikeys

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestWellFormed(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	good := []string{"acme_" + secret, "Acme_live_" + secret, "a_" + secret}
	for _, raw := range good {
		if !wellFormed(raw) {
			t.Errorf("wellFormed(%q) = false", raw)
		}
	}
	bad := []string{
		"",
		secret,                            // no prefix
		"acme" + secret,                   // no separator
		"acme_" + secret[:42],             // too short
		"acme_" + secret + "A",            // too long
		"1acme_" + secret,                 // prefix starts with a digit
		"ac-me_" + secret,                 // prefix with a hyphen
		"acme_" + secret[:42] + "B",       // non-canonical trailing bits
		"acme_" + strings.Repeat("!", 43), // not base64url
		strings.Repeat("a", 25) + "_" + secret,
	}
	for _, raw := range bad {
		if wellFormed(raw) {
			t.Errorf("wellFormed(%q) = true", raw)
		}
	}
}

func TestValidName(t *testing.T) {
	if name, err := validName("  Production  "); err != nil || name != "Production" {
		t.Fatalf("validName: %q %v", name, err)
	}
	for _, name := range []string{"", "   ", strings.Repeat("x", 65), "evil\u202Eexe", "tab\there", "\xff"} {
		if _, err := validName(name); err == nil {
			t.Errorf("validName(%q) succeeded", name)
		}
	}
	if _, err := validName(strings.Repeat("é", 64)); err != nil {
		t.Errorf("64 runes: %v", err)
	}
}
