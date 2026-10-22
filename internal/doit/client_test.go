package doit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericeng/tuber/internal/config"
)

func TestCreateAnnotationSuccessAndHeaders(t *testing.T) {
	var gotAuth, gotTenant, gotContentType string
	var gotReq CreateAnnotationRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/analytics/v1/annotations" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotContentType = r.Header.Get("Content-Type")

		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decoding request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(AnnotationResponse{
			ID:         "ann-123",
			Content:    gotReq.Content,
			Timestamp:  gotReq.Timestamp,
			Reports:    gotReq.Reports,
			CreateTime: "2026-10-21T12:00:00Z",
			UpdateTime: "2026-10-21T12:00:00Z",
		})
	}))
	defer srv.Close()

	client := NewClient(config.DoiTConfig{
		BaseURL:        srv.URL,
		APIToken:       "doit-secret-key",
		TenantID:       "customer-tenant-1",
		RateLimitRPS:   100,
		RateLimitBurst: 100,
		Timeout:        5 * time.Second,
		MaxRetries:     2,
	}, false)

	resp, err := client.CreateAnnotation(context.Background(), CreateAnnotationRequest{
		Content:   "[gcp-gke/prod] default/Deployment/api Scaled: replicas 2 -> 5",
		Timestamp: "2026-10-21T12:00:00Z",
		Reports:   []string{"rep-1"},
		Labels:    []string{"lbl-1"},
	})
	if err != nil {
		t.Fatalf("CreateAnnotation() error: %v", err)
	}

	if gotAuth != "Bearer doit-secret-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer doit-secret-key")
	}
	if gotTenant != "customer-tenant-1" {
		t.Errorf("X-Tenant-Id = %q, want %q", gotTenant, "customer-tenant-1")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if resp.ID != "ann-123" {
		t.Errorf("resp.ID = %q, want ann-123", resp.ID)
	}
	if len(gotReq.Reports) != 1 || gotReq.Reports[0] != "rep-1" {
		t.Errorf("gotReq.Reports = %v, want [rep-1]", gotReq.Reports)
	}
}

func TestCreateAnnotationRetriesOn429And503(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		switch n {
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"backend temporarily unavailable"}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(AnnotationResponse{
				ID:        "ann-recovered",
				Content:   "recovered",
				Timestamp: "2026-10-21T12:00:00Z",
			})
		}
	}))
	defer srv.Close()

	client := NewClient(config.DoiTConfig{
		BaseURL:        srv.URL,
		APIToken:       "token",
		RateLimitRPS:   100,
		RateLimitBurst: 100,
		MaxRetries:     3,
	}, false, WithBaseBackoff(5*time.Millisecond))

	resp, err := client.CreateAnnotation(context.Background(), CreateAnnotationRequest{
		Content:   "test retry",
		Timestamp: "2026-10-21T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("expected recovery after retries, got error: %v", err)
	}
	if resp.ID != "ann-recovered" {
		t.Errorf("resp.ID = %q, want ann-recovered", resp.ID)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestCreateAnnotationPermanentErrorFailsFast(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Cloud Analytics Admin permission required"}`))
	}))
	defer srv.Close()

	client := NewClient(config.DoiTConfig{
		BaseURL:        srv.URL,
		APIToken:       "token",
		RateLimitRPS:   100,
		RateLimitBurst: 100,
		MaxRetries:     3,
	}, false, WithBaseBackoff(5*time.Millisecond))

	_, err := client.CreateAnnotation(context.Background(), CreateAnnotationRequest{
		Content:   "test forbidden",
		Timestamp: "2026-10-21T12:00:00Z",
	})
	if err == nil {
		t.Fatal("expected error on 403 Forbidden, got nil")
	}
	if !IsPermanent(err) {
		t.Errorf("expected IsPermanent(err) == true, got false (%v)", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1 (no retries on 403)", attempts.Load())
	}
}

func TestDryRunDoesNotSendHTTPRequest(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(config.DoiTConfig{
		BaseURL: srv.URL,
	}, true)

	resp, err := client.CreateAnnotation(context.Background(), CreateAnnotationRequest{
		Content:   "dry run test",
		Timestamp: "2026-10-21T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("dry-run CreateAnnotation() error: %v", err)
	}
	if called.Load() {
		t.Error("expected dry-run client not to call HTTP server")
	}
	if resp.ID != "dry-run-annotation-id" {
		t.Errorf("resp.ID = %q, want dry-run-annotation-id", resp.ID)
	}
}
