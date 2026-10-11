package apikeys

import (
	"errors"
	"net/http"
	"slices"
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

// Mount adds the keys page to r, a group at the page's path that uses
// accounts.RequireSignIn, such as /account/api-keys:
//   - GET "" lists the account's keys.
//   - GET "/list" renders the list alone, for refreshes after a change.
//   - POST "" creates a key and shows it once.
//   - POST "/:id" renames ("action=rename" and "name"), revokes ("action=revoke") or
//     deletes ("action=delete" and the key's name as "confirm_name") a key. Only revoked
//     keys can be deleted.
func (k *Keys) Mount(r gin.IRouter, layout ui.Layout) {
	p := keysPages{k: k, layout: layout, base: basePath(r)}
	r.GET("", p.page)
	r.GET("/list", p.list)
	r.POST("", p.create)
	r.POST("/:id", p.edit)
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
	// created is the raw key just created, shown once, and createdName its name.
	created     string
	createdName string
	// createError reopens the create dialog with a message.
	createError string
	// failed means the list could not be loaded.
	failed bool
}

func (p keysPages) view(c *gin.Context) keysView {
	keys, err := p.k.List(c.Request.Context(), accounts.Current(c).ID)
	if err != nil {
		web.Log(c).Error("listing API keys failed", "error", err)
	}
	return keysView{c: c, base: p.base, keys: keys, failed: err != nil}
}

func htmx(c *gin.Context) bool {
	return c.GetHeader("HX-Request") == "true"
}

func (p keysPages) page(c *gin.Context) {
	p.layout.Render(c, http.StatusOK, "API keys", keysPage(p.view(c)))
}

func (p keysPages) list(c *gin.Context) {
	ui.Render(c, http.StatusOK, keyList(p.view(c), false))
}

const (
	msgNameInvalid  = "Name the key with 1 to 64 letters, numbers, spaces or punctuation."
	msgFailed       = "Something went wrong on our side. Try again in a moment."
	msgRenameGone   = "This key was revoked, so it can no longer be renamed. Reload the page to see its current state."
	msgDeleteName   = "Enter the exact key name."
	msgDeleteActive = "Revoke this key before deleting it."
)

func (p keysPages) create(c *gin.Context) {
	name := c.PostForm("name")
	raw, key, err := p.k.Create(c.Request.Context(), accounts.Current(c).ID, name)
	if errors.Is(err, ErrInvalidName) {
		if htmx(c) {
			ui.Render(c, http.StatusUnprocessableEntity, ui.FormError(msgNameInvalid))
			return
		}
		v := p.view(c)
		v.createError = msgNameInvalid
		p.layout.Render(c, http.StatusUnprocessableEntity, "API keys", keysPage(v))
		return
	}
	if err != nil {
		p.fail(c, err)
		return
	}
	v := p.view(c)
	v.created, v.createdName = raw, key.Name
	if htmx(c) {
		// The key exists now, so it is always shown; the list refreshes behind it.
		c.Header("HX-Retarget", "#key-created")
		c.Header("HX-Reswap", "innerHTML")
		c.Header("HX-Trigger", "dialog:close")
		ui.Render(c, http.StatusOK, createdFragment(v))
		return
	}
	p.layout.Render(c, http.StatusOK, "API keys", keysPage(v))
}

// edit renames, revokes or deletes a key. HTMX requests come from a dialog: errors go
// to its error slot, and success closes it and refreshes the list.
func (p keysPages) edit(c *gin.Context) {
	ctx := c.Request.Context()
	accountID := accounts.Current(c).ID
	keyID := c.Param("id")
	var status int
	var message string
	switch c.PostForm("action") {
	case "rename":
		err := p.k.Rename(ctx, accountID, keyID, c.PostForm("name"))
		switch {
		case errors.Is(err, ErrInvalidName):
			status, message = http.StatusUnprocessableEntity, msgNameInvalid
		case errors.Is(err, ErrNotFound):
			status, message = http.StatusConflict, msgRenameGone
		case err != nil:
			p.fail(c, err)
			return
		}
	case "revoke":
		// A key that is already revoked or gone no longer authenticates, which is what
		// was asked.
		if err := p.k.Revoke(ctx, accountID, keyID); err != nil && !errors.Is(err, ErrNotFound) {
			p.fail(c, err)
			return
		}
	case "delete":
		keys, err := p.k.List(ctx, accountID)
		if err != nil {
			p.fail(c, err)
			return
		}
		i := slices.IndexFunc(keys, func(k Key) bool { return k.ID == keyID })
		if i >= 0 && c.PostForm("confirm_name") != keys[i].Name {
			status, message = http.StatusUnprocessableEntity, msgDeleteName
			break
		}
		err = p.k.Delete(ctx, accountID, keyID)
		switch {
		case errors.Is(err, ErrActive):
			status, message = http.StatusConflict, msgDeleteActive
		case err != nil:
			p.fail(c, err)
			return
		}
	default:
		status, message = http.StatusBadRequest, "Something went wrong. Reload the page and try again."
	}
	switch {
	case message != "" && htmx(c):
		ui.Render(c, status, ui.FormError(message))
	case message != "":
		p.layout.Error(c, status, "The key was not changed", message)
	case htmx(c):
		c.Header("HX-Trigger", "dialog:close, api-keys-changed")
		c.Status(http.StatusNoContent)
	default:
		c.Redirect(http.StatusSeeOther, p.base)
	}
}

func (p keysPages) fail(c *gin.Context, err error) {
	web.Log(c).Error("API key page failed", "error", err)
	if htmx(c) {
		ui.Render(c, http.StatusInternalServerError, ui.FormError(msgFailed))
		return
	}
	p.layout.Error(c, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
}
