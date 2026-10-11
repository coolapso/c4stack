package hanko

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

// Mount adds the sign-in page at "login" under r, rendered inside layout. A user who
// is already signed in continues to the next path.
func (h *Hanko) Mount(r gin.IRouter, layout ui.Layout) {
	h.addAssets(layout.Assets)
	h.loginPath = joinPath(r, "/login")
	login := r.Group("/login", web.AllowCSP(h.csp(true)))
	login.GET("", h.login(layout))
}

func (h *Hanko) addAssets(assets *ui.Assets) {
	if err := assets.Add("hanko", assetFiles); err != nil {
		panic(err)
	}
}

// csp allows the browser SDK to reach Hanko. Hanko's elements inject inline styles,
// which only the sign-in page needs.
func (h *Hanko) csp(elements bool) web.CSP {
	p := web.CSP{Connect: []string{h.origin()}}
	if elements {
		p.Styles = []string{"'unsafe-inline'"}
	}
	return p
}

func (h *Hanko) login(layout ui.Layout) gin.HandlerFunc {
	return func(c *gin.Context) {
		next := c.Query("next")
		if !localPath(next) {
			next = h.signedInPath
		}
		if _, err := h.Authenticate(c.Request); err == nil {
			c.Redirect(http.StatusSeeOther, next)
			return
		}
		c.Header("Cache-Control", "no-store")
		layout.Render(c, http.StatusOK, "Sign in", loginPage(loginData{
			apiURL:    h.api.String(),
			moduleURL: layout.Assets.URL("hanko/elements.js"),
			scriptURL: layout.Assets.URL("hanko/login.js"),
			themeURL:  layout.Assets.URL("hanko/hanko.css"),
			next:      next,
		}))
	}
}

type loginData struct {
	apiURL    string
	moduleURL string
	scriptURL string
	themeURL  string
	next      string
}

// joinPath returns the full path of a route registered on r.
func joinPath(r gin.IRouter, p string) string {
	if g, ok := r.(interface{ BasePath() string }); ok {
		return strings.TrimSuffix(g.BasePath(), "/") + p
	}
	return p
}

// loginURL returns the sign-in page's URL that continues to next.
func (h *Hanko) loginURL(next string) string {
	if next == "" {
		return h.loginPath
	}
	return h.loginPath + "?" + url.Values{"next": {next}}.Encode()
}
