package email

import (
	"context"
	"fmt"
	"html"
	"io"
	"strings"

	"github.com/a-h/templ"
)

// Body is what an email says, written once and read as plain text and as HTML, so the
// two never say different things. The modules write their default emails with it.
type Body struct {
	parts []part
}

type partKind int

const (
	paragraph partKind = iota
	code
	link
)

type part struct {
	kind      partKind
	text, url string
}

// Paragraph adds a paragraph.
func (b *Body) Paragraph(text string) {
	b.parts = append(b.parts, part{kind: paragraph, text: text})
}

// Code adds something to copy, such as a sign-in code: indented in the text, a
// p.code in the HTML.
func (b *Body) Code(c string) {
	b.parts = append(b.parts, part{kind: code, text: c})
}

// Link adds a link to follow: "label: url" in the text, an a.button in the HTML. url
// must be absolute, http or https.
func (b *Body) Link(label, url string) {
	b.parts = append(b.parts, part{kind: link, text: label, url: url})
}

// Text returns the body in plain text: its parts with a blank line between them.
func (b Body) Text() string {
	var s strings.Builder
	for i, p := range b.parts {
		if i > 0 {
			s.WriteString("\n\n")
		}
		switch p.kind {
		case paragraph:
			s.WriteString(p.text)
		case code:
			s.WriteString("    " + p.text)
		case link:
			s.WriteString(p.text + ": " + p.url)
		}
	}
	s.WriteString("\n")
	return s.String()
}

// HTML returns the body in HTML, for a Layout to place: a p for each paragraph, a
// p.code for each code, and a p.action holding an a.button for each link. It has no
// styles of its own; the layout's give it the app's look.
func (b Body) HTML() templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var s strings.Builder
		for _, p := range b.parts {
			switch p.kind {
			case paragraph:
				s.WriteString("<p>" + html.EscapeString(p.text) + "</p>\n")
			case code:
				s.WriteString(`<p class="code">` + html.EscapeString(p.text) + "</p>\n")
			case link:
				s.WriteString(`<p class="action"><a class="button" href="` + html.EscapeString(string(templ.URL(p.url))) + `">` +
					html.EscapeString(p.text) + "</a></p>\n")
			}
		}
		_, err := io.WriteString(w, s.String())
		return err
	})
}

// Page is what a Layout puts in its document.
type Page struct {
	// Subject is the message's subject, for the document's title.
	Subject string
	// Preview is the start of the message's text, on one line and at most about 150
	// characters. Mail apps show the start of an email in their lists; a layout that
	// puts its own words first, such as the app's name in a header, puts Preview
	// before them, hidden, so the list shows the message.
	Preview string
	// Body is the message's body.
	Body templ.Component
}

// Layout makes an email's HTML document in the app's look: its head and styles, its
// header and footer, around the page's body. Mail apps take no stylesheets from the
// web and many keep only a style element in the head, so a layout styles the body's
// elements and classes (see Body.HTML) there, links nothing, and keeps to what mail
// apps render: tables for columns, no scripts, no web fonts it depends on. A Templ
// component with a Page parameter is one.
type Layout func(Page) templ.Component

// Compose writes a message from its subject and body: the text always, and the HTML
// too when there is a layout. It leaves To to the caller.
func Compose(ctx context.Context, subject string, body Body, layout Layout) (Message, error) {
	m := Message{Subject: subject, Text: body.Text()}
	if layout == nil {
		return m, nil
	}
	var doc strings.Builder
	page := Page{Subject: subject, Preview: preview(m.Text), Body: body.HTML()}
	if err := layout(page).Render(ctx, &doc); err != nil {
		return Message{}, fmt.Errorf("email: the layout failed: %w", err)
	}
	m.HTML = doc.String()
	return m, nil
}

// previewLength is about what mail apps show of an email in their lists.
const previewLength = 150

// preview returns the start of text on one line, cut after a word.
func preview(text string) string {
	words := strings.Fields(text)
	var s strings.Builder
	for _, w := range words {
		if s.Len() > 0 && s.Len()+1+len(w) > previewLength {
			return s.String() + "…"
		}
		if s.Len() > 0 {
			s.WriteByte(' ')
		}
		s.WriteString(w)
	}
	return s.String()
}
