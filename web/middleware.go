package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
)

// begin assigns a request ID and attaches the request state. Incoming request IDs are
// ignored, because clients could use them to forge or collide log entries.
func (s *settings) begin(c *gin.Context) {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	requestID := hex.EncodeToString(id)
	c.Header("X-Request-ID", requestID)
	c.Set(stateKey, &state{
		settings:  s,
		requestID: requestID,
		logger:    s.logger.With("request_id", requestID),
		policy:    s.policy,
	})
	c.Next()
}

// logRequest logs the route template, status and duration of every request. It never
// logs query strings, headers or bodies.
func (s *settings) logRequest(c *gin.Context) {
	start := time.Now()
	c.Next()

	status := c.Writer.Status()
	attrs := []any{
		"method", c.Request.Method,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if route := c.FullPath(); route != "" {
		attrs = append(attrs, "route", route)
	} else {
		path := c.Request.URL.Path
		if len(path) > 256 {
			path = path[:256]
		}
		attrs = append(attrs, "path", path)
	}
	level := slog.LevelInfo
	switch {
	case status >= 500:
		level = slog.LevelError
	case status >= 400:
		level = slog.LevelWarn
	}
	current(c).logger.Log(c.Request.Context(), level, "request", attrs...)
}

// recoverPanic turns a panic into a 500 response. The log has the route and the stack,
// but never the panic value or request headers, which can hold secrets.
func (s *settings) recoverPanic(c *gin.Context) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(v)
		}
		current(c).logger.Error("request panic",
			"method", c.Request.Method,
			"route", c.FullPath(),
			"stack", string(debug.Stack()),
		)
		if !c.Writer.Written() {
			c.AbortWithStatus(http.StatusInternalServerError)
		} else {
			c.Abort()
		}
	}()
	c.Next()
}

// checkHost rejects requests addressed to any host other than the public origin's.
func (s *settings) checkHost(c *gin.Context) {
	scheme := "http"
	if s.secure {
		scheme = "https"
	}
	host, err := canonicalHost(c.Request.Host, scheme)
	if err != nil || host != s.host {
		c.AbortWithStatus(http.StatusMisdirectedRequest)
		return
	}
	c.Next()
}

// secureHeaders sets the security headers and the Content Security Policy.
func (s *settings) secureHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("Content-Security-Policy", current(c).policy.String())
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if s.secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	c.Next()
}

// limitBody caps the request body. The cap is applied when the body is first read, so
// MaxBody on a route group can still change it.
func (s *settings) limitBody(c *gin.Context) {
	if c.Request.Body != nil && c.Request.Body != http.NoBody {
		c.Request.Body = &limitedBody{c: c, body: c.Request.Body, limit: s.maxBody}
	}
	c.Next()
}

// MaxBody sets the request body limit for the routes it is applied to.
func MaxBody(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if b, ok := c.Request.Body.(*limitedBody); ok && b.reader == nil {
			b.limit = n
		}
		c.Next()
	}
}

type limitedBody struct {
	c      *gin.Context
	body   io.ReadCloser
	limit  int64
	reader io.ReadCloser
	err    error
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		b.reader = http.MaxBytesReader(b.c.Writer, b.body, b.limit)
		// A declared length over the limit fails before anything is read.
		if b.c.Request.ContentLength > b.limit {
			b.err = &http.MaxBytesError{Limit: b.limit}
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.reader.Read(p)
}

func (b *limitedBody) Close() error {
	return b.body.Close()
}
