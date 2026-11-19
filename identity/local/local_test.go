package local

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/email"
)

type nopSender struct{}

func (nopSender) Send(context.Context, email.Message) (string, error) { return "", nil }

func newApp() *app.App {
	return app.New(app.Config{Name: "test", Migrations: []app.Migrations{Migrations}}, nil)
}

func TestConfigValidation(t *testing.T) {
	good := Config{AppName: "Acme", PublicURL: "https://acme.example", SecretKey: strings.Repeat("s", 32), Sender: nopSender{}}
	if _, err := New(newApp(), good); err != nil {
		t.Fatalf("a good configuration: %v", err)
	}
	for name, change := range map[string]func(*Config){
		"no app name":        func(c *Config) { c.AppName = "" },
		"no sender":          func(c *Config) { c.Sender = nil },
		"short secret":       func(c *Config) { c.SecretKey = "short" },
		"http public URL":    func(c *Config) { c.PublicURL = "http://acme.example" },
		"public URL path":    func(c *Config) { c.PublicURL = "https://acme.example/app" },
		"RPID with a scheme": func(c *Config) { c.RPID = "https://acme.example" },
		"RPID with a port":   func(c *Config) { c.RPID = "acme.example:443" },
		"RPID upper case":    func(c *Config) { c.RPID = "Acme.example" },
		"RPID elsewhere":     func(c *Config) { c.RPID = "other.example" },
		"origin outside":     func(c *Config) { c.Origins = []string{"https://other.example"} },
		"origin over http":   func(c *Config) { c.Origins = []string{"http://www.acme.example"} },
		"remote signed-in":   func(c *Config) { c.SignedInPath = "//evil.example" },
		"negative ceiling":   func(c *Config) { c.MaxCodesPerHour = -1 },
	} {
		cfg := good
		change(&cfg)
		if _, err := New(newApp(), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, change := range map[string]func(*Config){
		"a parent RPID":      func(c *Config) { c.PublicURL, c.RPID = "https://app.acme.example", "acme.example" },
		"a further origin":   func(c *Config) { c.Origins = []string{"https://www.acme.example"} },
		"loopback over http": func(c *Config) { c.PublicURL = "http://localhost:8080" },
	} {
		cfg := good
		change(&cfg)
		if _, err := New(newApp(), cfg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New(app.New(app.Config{Name: "test"}, nil), good); err == nil {
		t.Error("accepted without the migrations")
	}
}

func TestNormalizeAddress(t *testing.T) {
	for in, want := range map[string]string{
		" Ann@Example.COM ":                       "ann@example.com",
		"ann+tag@sub.example.org":                 "ann+tag@sub.example.org",
		"ann":                                     "",
		"Ann <ann@example.com>":                   "",
		"ann@example.com, b@x.org":                "",
		`"a b"@example.com`:                       "",
		"ann@[192.0.2.1]":                         "",
		"ann\n@example.com":                       "",
		"@example.com":                            "",
		strings.Repeat("a", 243) + "@example.com": "",
	} {
		got, ok := normalizeAddress(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := newCode()
		if len(c) != codeDigits || strings.Trim(c, "0123456789") != "" {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 195 {
		t.Fatal("codes repeat")
	}
	if got := formatCode("48210937"); got != "4821 0937" {
		t.Fatalf("formatted %q", got)
	}
	for in, want := range map[string]string{"4821 0937": "48210937", "4821-0937": "48210937", " 48210937 ": "48210937", "4821093": "", "4821O937": "", "482109371": ""} {
		got, ok := parseCode(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	l := &Local{codeKey: []byte(strings.Repeat("k", 32))}
	if string(l.codeHash("a", "12345678")) == string(l.codeHash("b", "12345678")) {
		t.Fatal("equal codes have equal hashes")
	}
}

func TestDeviceLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0":                                                                 "Firefox on Linux",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15":                  "Safari on macOS",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0":          "Edge on Windows",
		"Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36":                           "Chrome on Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/140.0 Mobile/15E148 Safari/604.1": "Chrome on iOS",
		"curl/8.0":           "An unknown device",
		"<script>x</script>": "An unknown device",
	} {
		if got := deviceLabel(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
}

func TestDefaultMessages(t *testing.T) {
	at := time.Date(2026, 11, 19, 14, 2, 0, 0, time.UTC)
	for k := SignInCode; k <= AddressSetBySupport; k++ {
		m := defaultMessage(Notice{
			Kind: k, To: "ann@example.com", AppName: "Acme", Site: "acme.example", Code: "4821 0937", Minutes: 10,
			OldEmail: "ann@example.com", NewEmail: "ann@example.org", Passkey: "Work laptop", Device: "Firefox on Linux",
			At: at, Until: at.Add(24 * time.Hour), Reference: "6f1c2e5a-0b7d-4c1e-9a53-2d8e4f7b1c90",
			SettingsURL: "https://acme.example/account/settings",
		})
		if m.Subject == "" || m.Text == "" || m.To != "ann@example.com" {
			t.Errorf("%v: empty message", k)
		}
		if !strings.Contains(m.Subject, "Acme") {
			t.Errorf("%v: the subject doesn't name the app: %q", k, m.Subject)
		}
		if strings.Contains(strings.ToLower(m.Subject+m.Text), "c4stack") {
			t.Errorf("%v names C4Stack", k)
		}
		if strings.Contains(m.Subject, "4821") {
			t.Errorf("%v: a code in the subject", k)
		}
		if k.String() == "" || strings.HasPrefix(k.String(), "Kind(") {
			t.Errorf("%v has no name", int(k))
		}
		if back, ok := kindNamed(k.String()); !ok || back != k {
			t.Errorf("%v: the name doesn't map back", k)
		}
	}
	m := defaultMessage(Notice{Kind: AddressChanged, AppName: "Acme", At: at, Reference: "ref-1"})
	if !strings.Contains(m.Text, "19 November 2026 at 14:02 UTC") || !strings.Contains(m.Text, "ref-1") {
		t.Fatalf("address changed:\n%s", m.Text)
	}
	if got := defaultMessage(Notice{Kind: SignUpCode, AppName: "Textlab"}).Text; !strings.Contains(got, "a Textlab account") {
		t.Fatalf("article: %s", got)
	}
}

// leakySender fails with everything the message says, as a careless adapter might.
type leakySender struct{}

func (leakySender) Send(_ context.Context, m email.Message) (string, error) {
	return "", fmt.Errorf("refused %s: %s %s", strings.ToUpper(m.To), m.Subject, m.Text)
}

func TestSenderErrorsCarryNothingTheEmailDoes(t *testing.T) {
	l := &Local{cfg: Config{Sender: leakySender{}, Message: defaultMessage}}
	for _, n := range []Notice{
		{Kind: SignInCode, To: "ann@example.com", AppName: "Acme", Code: "4821 0937"},
		{Kind: AddressChanged, To: "ann@example.com", AppName: "Acme", OldEmail: "ann@example.com", NewEmail: "ann@example.org"},
		{Kind: PasskeyAdded, To: "ann@example.com", AppName: "Acme", Passkey: "Ann's work laptop"},
	} {
		err := l.sendNow(context.Background(), n)
		if err == nil {
			t.Fatal("no error")
		}
		for _, secret := range []string{"ann@example", "4821", "0937", "Ann's work laptop"} {
			if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(secret)) {
				t.Errorf("%v: the error carries %q: %v", n.Kind, secret, err)
			}
		}
	}
}

func TestLocalPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/account": true, "/": true, "/a/../b?c=d": true,
		"//evil.example": false, `/\evil.example`: false, `/../\evil.example`: false, `/a/../../\evil.example`: false,
		"/\t/evil.example": false, "/a\r\nb": false, "/a\x00": false, "/a\x7f": false,
		"https://evil.example": false, "": false,
	} {
		if localPath(p) != want {
			t.Errorf("%q: %v", p, !want)
		}
	}
}
