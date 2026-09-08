// Package httpx holds the one HTTP client every provider uses: bounded
// timeouts, bounded retries and error bodies that are safe to log.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// DefaultTimeout bounds a single request including body read.
const DefaultTimeout = 30 * time.Second

// maxErrorBody is how much of a failing response we keep for the error message.
const maxErrorBody = 2048

// Client is a thin wrapper over http.Client with retries on transient failures.
type Client struct {
	HTTP       *http.Client
	UserAgent  string
	MaxRetries int
	// RetryPause is the base delay; attempt n waits RetryPause*2^n.
	RetryPause time.Duration
}

// New builds a client with a cookie jar, so providers that authenticate with a
// session cookie keep it across requests.
func New(userAgent string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		HTTP: &http.Client{
			Timeout: DefaultTimeout,
			Jar:     jar,
		},
		UserAgent:  userAgent,
		MaxRetries: 3,
		RetryPause: 500 * time.Millisecond,
	}
}

// StatusError is a non-2xx response.
type StatusError struct {
	StatusCode int
	Status     string
	URL        string
	Body       string
}

func (e *StatusError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("%s: %s", e.URL, e.Status)
	}
	return fmt.Sprintf("%s: %s: %s", e.URL, e.Status, body)
}

// Unauthorized reports whether the server rejected our credentials.
func (e *StatusError) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// NotFound reports whether the endpoint or resource does not exist, which for
// the reverse engineered providers usually means the API moved.
func (e *StatusError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// Do sends the request, retrying idempotent failures, and returns the body.
func (c *Client) Do(ctx context.Context, req *http.Request) ([]byte, *http.Response, error) {
	if c.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	var body []byte
	if req.Body != nil && req.GetBody == nil {
		// Buffer the body so a retry can replay it.
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, nil, err
		}
		req.Body.Close()
		body = raw
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, c.RetryPause<<uint(attempt-1)); err != nil {
				return nil, nil, err
			}
			if req.GetBody != nil {
				rc, err := req.GetBody()
				if err != nil {
					return nil, nil, err
				}
				req.Body = rc
			}
		}

		resp, err := c.HTTP.Do(req.WithContext(ctx))
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			continue
		}

		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return data, resp, nil
		}
		statusErr := &StatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			URL:        req.URL.Redacted(),
			Body:       truncate(string(data), maxErrorBody),
		}
		if !retryableStatus(resp.StatusCode) {
			return data, resp, statusErr
		}
		lastErr = statusErr
	}
	return nil, nil, lastErr
}

// GetJSON performs a GET and decodes the JSON body into out. The raw body is
// returned as well so providers can keep it for the caller.
func (c *Client) GetJSON(ctx context.Context, url string, headers map[string]string, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	data, _, err := c.Do(ctx, req)
	if err != nil {
		return data, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return data, fmt.Errorf("decode %s: %w (body starts with %q)", url, err, truncate(string(data), 200))
		}
	}
	return data, nil
}

// PostForm performs a form encoded POST and decodes the JSON response.
func (c *Client) PostForm(ctx context.Context, url string, form string, headers map[string]string, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	data, _, err := c.Do(ctx, req)
	if err != nil {
		return data, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return data, fmt.Errorf("decode %s: %w", url, err)
		}
	}
	return data, nil
}

// PostJSON performs a JSON POST and decodes the JSON response.
func (c *Client) PostJSON(ctx context.Context, url string, payload any, headers map[string]string, out any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	data, _, err := c.Do(ctx, req)
	if err != nil {
		return data, err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return data, fmt.Errorf("decode %s: %w", url, err)
		}
	}
	return data, nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
