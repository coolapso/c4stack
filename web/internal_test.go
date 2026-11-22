package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestNewInternalValidatesConfig(t *testing.T) {
	for name, hashes := range map[string][]string{
		"no hash":   nil,
		"not hex":   {strings.Repeat("z", 64)},
		"too short": {"abcd"},
		"the token": {"a-token-not-its-hash"},
	} {
		if _, err := NewInternal(InternalConfig{TokenHashes: hashes}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := NewInternal(InternalConfig{TokenHashes: []string{strings.ToUpper(tokenHash("t")) + "\n"}}); err != nil {
		t.Errorf("an upper-case hash with a newline: %v", err)
	}
}

// TestInternalToken: only a token whose hash is listed gets through, any of them, and
// nothing of the header reaches the log; any host is fine, and browsers' cookies,
// headers and policy are absent.
func TestInternalToken(t *testing.T) {
	var logs bytes.Buffer
	r, err := NewInternal(InternalConfig{
		TokenHashes: []string{tokenHash("old-token"), tokenHash("new-token")},
		Logger:      slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.GET("/v1/things", func(c *gin.Context) {
		Log(c).Info("handled")
		c.Status(http.StatusNoContent)
	})

	for header, want := range map[string]int{
		"":                         http.StatusUnauthorized,
		"Bearer":                   http.StatusUnauthorized,
		"Bearer ":                  http.StatusUnauthorized,
		"Bearer wrong-token":       http.StatusUnauthorized,
		"Basic b2xkLXRva2Vu":       http.StatusUnauthorized,
		"bearer new-token":         http.StatusUnauthorized,
		"Bearer " + tokenHash("x"): http.StatusUnauthorized,
		"Bearer old-token":         http.StatusNoContent,
		"Bearer new-token":         http.StatusNoContent,
	} {
		h := http.Header{}
		if header != "" {
			h.Set("Authorization", header)
		}
		rec := request(r, http.MethodGet, "http://account-internal.default.svc:8081/v1/things", nil, h)
		if rec.Code != want {
			t.Errorf("%q: status %d, want %d", header, rec.Code, want)
		}
		if want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: no WWW-Authenticate", header)
		}
		if len(rec.Header().Get("X-Request-ID")) != 32 {
			t.Errorf("%q: no request ID", header)
		}
		for _, browserOnly := range []string{"Set-Cookie", "Content-Security-Policy", "X-Frame-Options"} {
			if rec.Header().Get(browserOnly) != "" {
				t.Errorf("%q: %s set", header, browserOnly)
			}
		}
	}
	out := logs.String()
	if !strings.Contains(out, "handled") || !strings.Contains(out, "route=/v1/things") || !strings.Contains(out, "status=401") {
		t.Fatalf("unexpected log: %s", out)
	}
	for _, secret := range []string{"old-token", "new-token", "wrong-token"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

// TestInternalLimitsBodies: the body limit applies, as on New's engine.
func TestInternalLimitsBodies(t *testing.T) {
	r, err := NewInternal(InternalConfig{TokenHashes: []string{tokenHash("t")}, MaxBodyBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	r.POST("/v1/things", func(c *gin.Context) {
		var v map[string]any
		if err := c.ShouldBindJSON(&v); err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusNoContent)
	})
	h := http.Header{"Authorization": {"Bearer t"}, "Content-Type": {"application/json"}}
	if rec := request(r, http.MethodPost, "http://internal/v1/things", strings.NewReader(`{"a":1}`), h); rec.Code != http.StatusNoContent {
		t.Fatalf("a small body: %d", rec.Code)
	}
	if rec := request(r, http.MethodPost, "http://internal/v1/things", strings.NewReader(`{"a":"longer than eight"}`), h); rec.Code == http.StatusNoContent {
		t.Fatalf("a large body: %d", rec.Code)
	}
}
