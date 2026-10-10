package web

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server describes how Serve runs an http.Handler.
type Server struct {
	// Addr is the listen address, such as ":8080".
	Addr    string
	Handler http.Handler
	// HealthPath serves HealthPath+"/live" and HealthPath+"/ready" in front of Handler,
	// so probes skip the host check and the request log. Empty disables them.
	HealthPath string
	// DrainDelay is how long readiness fails before the listener stops accepting
	// requests, so load balancers stop routing first. Zero means 5 seconds.
	DrainDelay time.Duration
	// ShutdownTimeout bounds how long in-flight requests get to finish. Zero means
	// 30 seconds.
	ShutdownTimeout time.Duration
	// Logger receives startup and shutdown messages. Zero means slog.Default().
	Logger *slog.Logger
}

// Serve runs s until ctx is cancelled, then shuts down gracefully: readiness fails
// first, the listener stays open for DrainDelay, and in-flight requests get
// ShutdownTimeout to finish. ready reports whether the app can take traffic.
func Serve(ctx context.Context, s Server, ready func(context.Context) error) error {
	if s.DrainDelay == 0 {
		s.DrainDelay = 5 * time.Second
	}
	if s.ShutdownTimeout == 0 {
		s.ShutdownTimeout = 30 * time.Second
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var draining atomic.Bool
	handler := s.Handler
	if s.HealthPath != "" {
		handler = health(s.HealthPath, s.Handler, &draining, ready)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", ln.Addr().String())

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	logger.Info("draining", "delay", s.DrainDelay.String())
	draining.Store(true)
	time.Sleep(s.DrainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("in-flight requests did not finish in time", "timeout", s.ShutdownTimeout.String())
		_ = srv.Close()
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}

func health(path string, next http.Handler, draining *atomic.Bool, ready func(context.Context) error) http.Handler {
	live, readyPath := path+"/live", path+"/ready"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case live:
			w.WriteHeader(http.StatusNoContent)
		case readyPath:
			if draining.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if ready != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				defer cancel()
				if err := ready(ctx); err != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
