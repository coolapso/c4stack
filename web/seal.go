package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// ErrNoCookie is returned by SealedCookie when the cookie is missing, expired or was
// not sealed by this app.
var ErrNoCookie = errors.New("web: no valid sealed cookie")

// SetSealedCookie stores value in an encrypted, authenticated cookie that expires after
// maxAge. Use it for short-lived server state, not for large data: browsers limit
// cookies to about 4 KB.
func SetSealedCookie(c *gin.Context, name string, value []byte, maxAge time.Duration) {
	s := current(c).settings
	expires := time.Now().Add(maxAge)
	plain := binary.BigEndian.AppendUint64(nil, uint64(expires.Unix()))
	plain = append(plain, value...)
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     s.cookieName(name),
		Value:    base64.RawURLEncoding.EncodeToString(s.seal(name, plain)),
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// SealedCookie returns the value stored by SetSealedCookie.
func SealedCookie(c *gin.Context, name string) ([]byte, error) {
	s := current(c).settings
	raw, err := c.Cookie(s.cookieName(name))
	if err != nil {
		return nil, ErrNoCookie
	}
	sealed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrNoCookie
	}
	plain, err := s.open(name, sealed)
	if err != nil || len(plain) < 8 {
		return nil, ErrNoCookie
	}
	if time.Now().Unix() > int64(binary.BigEndian.Uint64(plain[:8])) {
		return nil, ErrNoCookie
	}
	return plain[8:], nil
}

// ClearSealedCookie deletes a cookie set by SetSealedCookie.
func ClearSealedCookie(c *gin.Context, name string) {
	s := current(c).settings
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     s.cookieName(name),
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// seal encrypts plain with AES-GCM. The cookie name is authenticated too, so a value
// cannot be moved to another cookie.
func (s *settings) seal(name string, plain []byte) []byte {
	aead := s.aead()
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, []byte(name))
}

func (s *settings) open(name string, sealed []byte) ([]byte, error) {
	aead := s.aead()
	if len(sealed) < aead.NonceSize() {
		return nil, ErrNoCookie
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, ciphertext, []byte(name))
}

func (s *settings) aead() cipher.AEAD {
	block, err := aes.NewCipher(s.cookieKey)
	if err != nil {
		panic(err) // the key is always 32 bytes
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return aead
}
