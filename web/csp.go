package web

import (
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
)

// CSP lists Content Security Policy sources. Each field adds sources to one directive of
// the default policy, which allows only the app's own origin.
type CSP struct {
	Scripts     []string // script-src
	Styles      []string // style-src
	Images      []string // img-src
	Fonts       []string // font-src
	Connect     []string // connect-src
	Frames      []string // frame-src
	FormActions []string // form-action
}

func defaultCSP() CSP {
	return CSP{
		Scripts:     []string{"'self'"},
		Styles:      []string{"'self'"},
		Images:      []string{"'self'", "data:"},
		Fonts:       []string{"'self'"},
		Connect:     []string{"'self'"},
		Frames:      []string{"'none'"},
		FormActions: []string{"'self'"},
	}
}

// with returns the policy with extra's sources added. Adding sources to a directive
// that only allows 'none' replaces 'none'.
func (p CSP) with(extra CSP) CSP {
	merge := func(base, add []string) []string {
		out := slices.Clone(base)
		for _, src := range add {
			if src == "" || slices.Contains(out, src) {
				continue
			}
			if len(out) == 1 && out[0] == "'none'" {
				out = out[:0]
			}
			out = append(out, src)
		}
		return out
	}
	return CSP{
		Scripts:     merge(p.Scripts, extra.Scripts),
		Styles:      merge(p.Styles, extra.Styles),
		Images:      merge(p.Images, extra.Images),
		Fonts:       merge(p.Fonts, extra.Fonts),
		Connect:     merge(p.Connect, extra.Connect),
		Frames:      merge(p.Frames, extra.Frames),
		FormActions: merge(p.FormActions, extra.FormActions),
	}
}

// String renders the policy as a Content-Security-Policy header value.
func (p CSP) String() string {
	directives := []struct {
		name    string
		sources []string
	}{
		{"default-src", []string{"'self'"}},
		{"script-src", p.Scripts},
		{"style-src", p.Styles},
		{"img-src", p.Images},
		{"font-src", p.Fonts},
		{"connect-src", p.Connect},
		{"frame-src", p.Frames},
		{"form-action", p.FormActions},
		{"frame-ancestors", []string{"'none'"}},
		{"base-uri", []string{"'none'"}},
		{"object-src", []string{"'none'"}},
	}
	parts := make([]string, 0, len(directives))
	for _, d := range directives {
		parts = append(parts, d.name+" "+strings.Join(d.sources, " "))
	}
	return strings.Join(parts, "; ")
}

// AllowCSP adds sources to the Content Security Policy of the routes it is applied to.
// Modules use it on the groups they mount, for example to let a login page reach its
// identity provider.
func AllowCSP(extra CSP) gin.HandlerFunc {
	return func(c *gin.Context) {
		st := current(c)
		st.policy = st.policy.with(extra)
		c.Header("Content-Security-Policy", st.policy.String())
		c.Next()
	}
}
