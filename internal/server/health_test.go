package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthAndReadyEndpoints(t *testing.T) {
	var synced atomic.Bool
	handler := NewHealthHandler(func() bool {
		return synced.Load()
	})

	// 1. /healthz is always 200 OK.
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, reqHealth)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", recHealth.Code)
	}
	if got := recHealth.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recHealth.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	// 2. /readyz before sync is 503 Service Unavailable.
	reqReady1 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recReady1 := httptest.NewRecorder()
	handler.ServeHTTP(recReady1, reqReady1)
	if recReady1.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before sync status = %d, want 503", recReady1.Code)
	}

	// 3. /readyz after sync is 200 OK.
	synced.Store(true)
	reqReady2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	recReady2 := httptest.NewRecorder()
	handler.ServeHTTP(recReady2, reqReady2)
	if recReady2.Code != http.StatusOK {
		t.Fatalf("/readyz after sync status = %d, want 200", recReady2.Code)
	}
}
