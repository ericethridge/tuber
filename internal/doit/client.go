package doit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ericethridge/tuber/internal/config"
	"golang.org/x/time/rate"
)

const (
	annotationsPath = "/analytics/v1/annotations"
	userAgent       = "tuber-k8s-monitor/1.0"
)

// CreateAnnotationRequest matches the DoiT Annotations API CreateAnnotationRequest schema.
type CreateAnnotationRequest struct {
	Content   string   `json:"content"`
	Timestamp string   `json:"timestamp"`
	Reports   []string `json:"reports,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

// LabelInfo represents a label metadata object in DoiT API responses.
type LabelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AnnotationResponse matches the DoiT Annotations API AnnotationListItem schema.
type AnnotationResponse struct {
	ID         string      `json:"id"`
	Content    string      `json:"content"`
	Timestamp  string      `json:"timestamp"`
	Reports    []string    `json:"reports,omitempty"`
	Labels     []LabelInfo `json:"labels,omitempty"`
	CreateTime string      `json:"createTime,omitempty"`
	UpdateTime string      `json:"updateTime,omitempty"`
}

// errorResponse matches DoiT's standard error JSON payload.
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// APIError represents an HTTP error returned by the DoiT API.
type APIError struct {
	StatusCode int
	Message    string
	Permanent  bool
}

func (e *APIError) Error() string {
	kind := "transient"
	if e.Permanent {
		kind = "permanent"
	}
	return fmt.Sprintf("doit api %s error (HTTP %d): %s", kind, e.StatusCode, e.Message)
}

// IsPermanent returns true if the error is a non-retryable 4xx API error.
func IsPermanent(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Permanent
	}
	return false
}

// Sink is the interface for delivering formatted DoiT annotations.
type Sink interface {
	CreateAnnotation(ctx context.Context, req CreateAnnotationRequest) (*AnnotationResponse, error)
	Ping(ctx context.Context) error
}

// Client is an HTTP client for the DoiT Cloud Intelligence Annotations API.
type Client struct {
	baseURL     string
	apiToken    string
	tenantID    string
	dryRun      bool
	maxRetries  int
	baseBackoff time.Duration
	limiter     *rate.Limiter
	httpClient  *http.Client
	logger      *slog.Logger
}

// Option customizes a Client instance (primarily for testing).
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithBaseBackoff overrides the initial retry backoff duration.
func WithBaseBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.baseBackoff = d
		}
	}
}

// WithLogger sets a structured logger on the Client.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.logger = l
		}
	}
}

// NewClient constructs a DoiT Annotations API client from configuration.
func NewClient(cfg config.DoiTConfig, dryRun bool, opts ...Option) *Client {
	rps := cfg.RateLimitRPS
	if rps <= 0 {
		rps = config.DefaultRateLimitRPS
	}
	burst := cfg.RateLimitBurst
	if burst <= 0 {
		burst = config.DefaultRateLimitBurst
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = config.DefaultHTTPTimeout
	}

	c := &Client{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		apiToken:    cfg.APIToken,
		tenantID:    cfg.TenantID,
		dryRun:      dryRun,
		maxRetries:  cfg.MaxRetries,
		baseBackoff: 250 * time.Millisecond,
		limiter:     rate.NewLimiter(rate.Limit(rps), burst),
		httpClient: &http.Client{
			Timeout: timeout,
		},
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CreateAnnotation posts a new annotation to POST /analytics/v1/annotations.
func (c *Client) CreateAnnotation(ctx context.Context, req CreateAnnotationRequest) (*AnnotationResponse, error) {
	if strings.TrimSpace(req.Content) == "" {
		return nil, errors.New("annotation content cannot be empty")
	}
	if strings.TrimSpace(req.Timestamp) == "" {
		return nil, errors.New("annotation timestamp cannot be empty")
	}

	if c.dryRun {
		c.logger.Info("dry-run: would create DoiT annotation",
			"content", req.Content,
			"timestamp", req.Timestamp,
			"reports", req.Reports,
			"labels", req.Labels,
			"tenantId", c.tenantID,
		)
		return &AnnotationResponse{
			ID:        "dry-run-annotation-id",
			Content:   req.Content,
			Timestamp: req.Timestamp,
			Reports:   req.Reports,
		}, nil
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling CreateAnnotationRequest: %w", err)
	}

	endpoint := c.baseURL + annotationsPath
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.computeBackoff(attempt, lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter wait: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("creating HTTP request: %w", err)
		}
		c.setHeaders(httpReq)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("executing HTTP POST %s: %w", annotationsPath, err)
			continue
		}

		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("reading response body: %w", readErr)
			continue
		}

		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
			var out AnnotationResponse
			if len(bodyBytes) > 0 {
				if err := json.Unmarshal(bodyBytes, &out); err != nil {
					return nil, fmt.Errorf("decoding DoiT annotation response: %w", err)
				}
			}
			return &out, nil
		}

		apiErr := parseAPIError(resp, bodyBytes)
		if apiErr.Permanent {
			return nil, apiErr
		}
		lastErr = apiErr
	}

	return nil, lastErr
}

// Ping verifies credentials and connectivity via GET /analytics/v1/annotations?maxResults=1.
func (c *Client) Ping(ctx context.Context) error {
	if c.dryRun && strings.TrimSpace(c.apiToken) == "" {
		return nil
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}

	endpoint := c.baseURL + annotationsPath + "?maxResults=1"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating Ping request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("executing Ping GET %s: %w", annotationsPath, err)
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return parseAPIError(resp, bodyBytes)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if c.tenantID != "" {
		req.Header.Set("X-Tenant-Id", c.tenantID)
	}
}

type retryAfterError struct {
	*APIError
	retryAfter time.Duration
}

func parseAPIError(resp *http.Response, body []byte) *APIError {
	msg := strings.TrimSpace(string(body))
	var parsed errorResponse
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Error != "" {
			msg = parsed.Error
		} else if parsed.Message != "" {
			msg = parsed.Message
		}
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}

	permanent := resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusNotFound

	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				msg = fmt.Sprintf("%s (retry-after=%ds)", msg, secs)
			}
		}
	}

	return &APIError{
		StatusCode: resp.StatusCode,
		Message:    msg,
		Permanent:  permanent,
	}
}

func (c *Client) computeBackoff(attempt int, _ error) time.Duration {
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	base := c.baseBackoff * time.Duration(1<<shift)
	jitter := time.Duration(rand.Int64N(int64(c.baseBackoff)/2 + 1))
	return base + jitter
}
