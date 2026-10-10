package apikeys

import (
	"errors"
	"net/http"
	"strings"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/ui"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
)

const keyContext = "c4stack/apikeys"

// RequireKey protects API routes. It accepts exactly one "Authorization: Bearer <key>"
// header, ignores cookies, and answers 401 with a JSON error otherwise. The header is
// removed once the key is checked, so handlers cannot log or forward it.
func (k *Keys) RequireKey(c *gin.Context) {
	values := c.Request.Header.Values("Authorization")
	if len(values) == 0 {
		unauthorized(c, "missing_api_key", "Send your API key in the Authorization header as: Bearer <key>")
		return
	}
	fields := strings.Fields(values[0])
	if len(values) != 1 || len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		unauthorized(c, "invalid_api_key", "The Authorization header must be: Bearer <key>")
		return
	}
	key, err := k.Authenticate(c.Request.Context(), fields[1])
	if errors.Is(err, ErrInvalidKey) {
		unauthorized(c, "invalid_api_key", "The API key is not valid or was revoked.")
		return
	}
	if err != nil {
		web.Log(c).Error("API key check failed", "error", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable", "message": "Try again in a moment."})
		return
	}
	c.Request.Header.Del("Authorization")
	c.Header("Cache-Control", "no-store")
	c.Set(keyContext, key)
}

func unauthorized(c *gin.Context, code, message string) {
	c.Header("WWW-Authenticate", `Bearer realm="api"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": code, "message": message})
}

// Current returns the key that authenticated the request. It panics on routes without
// RequireKey, which is a programming error.
func Current(c *gin.Context) Key {
	v, ok := c.Get(keyContext)
	if !ok {
		panic("apikeys: Current needs a route behind RequireKey")
	}
	return v.(Key)
}

// Mount adds the keys page under r, which must use accounts.RequireSignIn:
//   - GET "" lists the account's keys.
//   - POST "" creates a key and shows it once.
//   - POST "/:id/revoke" revokes a key.
func (k *Keys) Mount(r gin.IRouter, layout ui.Layout) {
	p := keysPages{k: k, layout: layout, base: basePath(r)}
	r.GET("", p.list)
	r.POST("", p.create)
	r.POST("/:id/revoke", p.revoke)
}

func basePath(r gin.IRouter) string {
	if g, ok := r.(interface{ BasePath() string }); ok {
		return strings.TrimSuffix(g.BasePath(), "/")
	}
	return ""
}

type keysPages struct {
	k      *Keys
	layout ui.Layout
	base   string
}

// keysView is what the keys page shows.
type keysView struct {
	c    *gin.Context
	base string
	keys []Key
	// created is the raw key just created, shown once.
	created string
	// createError reopens the create dialog with a message.
	createError string
}

func (p keysPages) view(c *gin.Context) (keysView, error) {
	keys, err := p.k.List(c.Request.Context(), accounts.Current(c).ID)
	return keysView{c: c, base: p.base, keys: keys}, err
}

func (p keysPages) list(c *gin.Context) {
	v, err := p.view(c)
	if err != nil {
		p.fail(c, err)
		return
	}
	p.layout.Render(c, http.StatusOK, "API keys", keysPage(v))
}

func (p keysPages) create(c *gin.Context) {
	htmx := c.GetHeader("HX-Request") == "true"
	raw, _, err := p.k.Create(c.Request.Context(), accounts.Current(c).ID, c.PostForm("name"))
	if errors.Is(err, ErrInvalidName) {
		if htmx {
			ui.Render(c, http.StatusUnprocessableEntity, ui.FormError("Name the key with 1 to 64 characters."))
			return
		}
		v, err := p.view(c)
		if err != nil {
			p.fail(c, err)
			return
		}
		v.createError = "Name the key with 1 to 64 characters."
		p.layout.Render(c, http.StatusUnprocessableEntity, "API keys", keysPage(v))
		return
	}
	if err != nil {
		p.fail(c, err)
		return
	}
	v, err := p.view(c)
	if err != nil {
		p.fail(c, err)
		return
	}
	v.created = raw
	if htmx {
		c.Header("HX-Retarget", "#key-modal")
		c.Header("HX-Reswap", "innerHTML")
		c.Header("HX-Trigger", "dialog:close")
		ui.Render(c, http.StatusOK, createdFragment(v))
		return
	}
	p.layout.Render(c, http.StatusOK, "API keys", keysPage(v))
}

func (p keysPages) revoke(c *gin.Context) {
	err := p.k.Revoke(c.Request.Context(), accounts.Current(c).ID, c.Param("id"))
	if err != nil && !errors.Is(err, ErrNotFound) {
		p.fail(c, err)
		return
	}
	if c.GetHeader("HX-Request") != "true" {
		c.Redirect(http.StatusSeeOther, p.base)
		return
	}
	v, err := p.view(c)
	if err != nil {
		p.fail(c, err)
		return
	}
	ui.Render(c, http.StatusOK, keyList(v, false))
}

func (p keysPages) fail(c *gin.Context, err error) {
	web.Log(c).Error("API key page failed", "error", err)
	if c.GetHeader("HX-Request") == "true" {
		ui.Render(c, http.StatusInternalServerError, ui.FormError("Something went wrong on our side. Try again in a moment."))
		return
	}
	p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
}
