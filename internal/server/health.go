package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// NewHealthHandler returns an http.Handler serving /healthz (liveness) and /readyz (readiness).
func NewHealthHandler(readyFn func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if readyFn != nil && readyFn() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: waiting for informer cache sync\n"))
	})
	return mux
}

// HealthServer runs the HTTP liveness and readiness probe server.
type HealthServer struct {
	addr    string
	readyFn func() bool
	logger  *slog.Logger
}

// NewHealthServer creates a new HealthServer bound to addr (e.g. ":8080").
func NewHealthServer(addr string, readyFn func() bool, logger *slog.Logger) *HealthServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &HealthServer{
		addr:    addr,
		readyFn: readyFn,
		logger:  logger,
	}
}

// Start listens and serves /healthz and /readyz until ctx is canceled.
func (s *HealthServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           NewHealthHandler(s.readyFn),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.logger.Info("starting health/readiness server", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
