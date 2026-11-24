package email

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func sample() Body {
	var b Body
	b.Paragraph("Hello,")
	b.Paragraph(`Here is your code for "Ann & Bob's" <laptop>:`)
	b.Code("4821 0937")
	b.Link("Open your settings", "https://acme.example/account/settings?a=1&b=2")
	return b
}

func TestBodyText(t *testing.T) {
	want := "Hello,\n\nHere is your code for \"Ann & Bob's\" <laptop>:\n\n    4821 0937\n\n" +
		"Open your settings: https://acme.example/account/settings?a=1&b=2\n"
	if got := sample().Text(); got != want {
		t.Fatalf("text:\n%q\nwant\n%q", got, want)
	}
}

func TestBodyHTML(t *testing.T) {
	var s strings.Builder
	if err := sample().HTML().Render(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	want := "<p>Hello,</p>\n" +
		"<p>Here is your code for &#34;Ann &amp; Bob&#39;s&#34; &lt;laptop&gt;:</p>\n" +
		"<p class=\"code\">4821 0937</p>\n" +
		"<p class=\"action\"><a class=\"button\" href=\"https://acme.example/account/settings?a=1&amp;b=2\">Open your settings</a></p>\n"
	if s.String() != want {
		t.Fatalf("html:\n%s\nwant\n%s", s.String(), want)
	}

	var b Body
	b.Link("Click", "javascript:alert(1)")
	s.Reset()
	_ = b.HTML().Render(context.Background(), &s)
	if strings.Contains(s.String(), "javascript") {
		t.Fatalf("an unsafe link: %s", s.String())
	}
}

func TestPreview(t *testing.T) {
	for text, want := range map[string]string{
		"":                          "",
		"Hello,\n\n    4821 0937\n": "Hello, 4821 0937",
		strings.Repeat("word ", 40): strings.TrimSpace(strings.Repeat("word ", 30)) + "…",
		strings.Repeat("x", 200):    strings.Repeat("x", 200),
	} {
		if got := preview(text); got != want {
			t.Errorf("preview(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestCompose(t *testing.T) {
	ctx := context.Background()
	m, err := Compose(ctx, "Your code", sample(), nil)
	if err != nil || m.Subject != "Your code" || m.Text != sample().Text() || m.HTML != "" || m.To != "" {
		t.Fatalf("without a layout: %+v %v", m, err)
	}

	var page Page
	layout := func(p Page) templ.Component {
		page = p
		return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			io.WriteString(w, "<!DOCTYPE html><title>"+p.Subject+"</title>")
			return p.Body.Render(ctx, w)
		})
	}
	m, err = Compose(ctx, "Your code", sample(), layout)
	if err != nil || m.Text != sample().Text() || !strings.HasPrefix(m.HTML, "<!DOCTYPE html><title>Your code</title><p>Hello,</p>") {
		t.Fatalf("with a layout: %+v %v", m, err)
	}
	if page.Preview != `Hello, Here is your code for "Ann & Bob's" <laptop>: 4821 0937 Open your settings: https://acme.example/account/settings?a=1&b=2` {
		t.Fatalf("preview %q", page.Preview)
	}

	failing := func(Page) templ.Component {
		return templ.ComponentFunc(func(context.Context, io.Writer) error { return errors.New("broken") })
	}
	if _, err := Compose(ctx, "Your code", sample(), failing); err == nil {
		t.Fatal("a failing layout composed a message")
	}
}
