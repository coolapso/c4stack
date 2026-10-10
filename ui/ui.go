// Package ui holds the default design and the types default pages use to render inside
// an app's layout.
//
// Apps choose how much of the default design to keep:
//   - Use it unchanged with Default.
//   - Restyle it: link a stylesheet that overrides the design tokens, and set the
//     favicon, navigation and footer through Design.
//   - Replace it: write a Layout with the app's own markup and CSS. Default pages then
//     render inside it, using the component classes base.css defines.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/gin-gonic/gin"
)

//go:embed files
var embedded embed.FS

func files() fs.FS {
	sub, err := fs.Sub(embedded, "files")
	if err != nil {
		panic(err)
	}
	return sub
}

// Layout is everything a default page needs from the app's design.
type Layout struct {
	// Page wraps a page's body in the app's document: head, navigation and footer.
	Page func(c *gin.Context, title string, body templ.Component) templ.Component
	// Assets serves the stylesheets and scripts pages link to.
	Assets *Assets
}

// Render writes a full page with the given status.
func (l Layout) Render(c *gin.Context, status int, title string, body templ.Component) {
	Render(c, status, l.Page(c, title, body))
}

// Render writes an HTML component, such as an HTMX fragment, with the given status.
func Render(c *gin.Context, status int, component templ.Component) {
	c.Status(status)
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(c.Request.Context(), c.Writer); err != nil {
		_ = c.Error(err)
	}
}

// Link is a navigation link.
type Link struct {
	Label string
	Href  string
}

// Design customises the default layout.
type Design struct {
	// Name appears in the header and in every page title.
	Name string
	// Nav links appear in the header. The link matching the current path is marked.
	Nav []Link
	// Head is extra content for <head>, such as a favicon or meta tags.
	Head templ.Component
	// Stylesheets are linked after the default stylesheet, so they can override its
	// design tokens.
	Stylesheets []string
	// Footer replaces the default footer.
	Footer templ.Component
}

// Default returns the default layout with the given customisations.
func Default(d Design, assets *Assets) Layout {
	return Layout{
		Assets: assets,
		Page: func(c *gin.Context, title string, body templ.Component) templ.Component {
			return page(d, assets, c, title, body)
		},
	}
}

// current reports whether a navigation link points at the request's path.
func current(c *gin.Context, href string) bool {
	path := c.Request.URL.Path
	if href == "/" {
		return path == "/"
	}
	return path == href || strings.HasPrefix(path, strings.TrimSuffix(href, "/")+"/")
}

func pageTitle(title, name string) string {
	if title == "" || title == name {
		return name
	}
	return title + " · " + name
}

// NotFound renders a 404 page in the layout. Apps can use it as their NoRoute handler.
func (l Layout) NotFound(c *gin.Context) {
	l.Render(c, http.StatusNotFound, "Page not found", message("Page not found", "The page you asked for does not exist."))
}

// Error renders a short message page in the layout with the given status.
func (l Layout) Error(c *gin.Context, status int, title, text string) {
	l.Render(c, status, title, message(title, text))
}
