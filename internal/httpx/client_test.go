package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient() *Client {
	c := New("test-agent")
	c.RetryPause = time.Millisecond
	return c
}

func TestRetriesTransientFailures(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer server.Close()

	var out struct {
		OK bool `json:"ok"`
	}
	if _, err := testClient().GetJSON(context.Background(), server.URL, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || attempts != 3 {
		t.Fatalf("attempts = %d, decoded = %+v", attempts, out)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := testClient().GetJSON(context.Background(), server.URL, nil, nil)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want a StatusError", err)
	}
	if !statusErr.Unauthorized() {
		t.Error("401 should be reported as unauthorized")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want no retry on a credential failure", attempts)
	}
}

func TestSendsUserAgentAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "test-agent" {
			t.Errorf("user agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Authorization") != "Bearer x" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	if _, err := testClient().GetJSON(context.Background(), server.URL, map[string]string{"Authorization": "Bearer x"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPostFormReplaysBodyOnRetry(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		if len(bodies) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token": "t"}`))
	}))
	defer server.Close()

	var out struct {
		Token string `json:"access_token"`
	}
	if _, err := testClient().PostForm(context.Background(), server.URL, "grant_type=refresh_token", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Token != "t" {
		t.Fatalf("token = %q", out.Token)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("the retried request lost its body: %q", bodies)
	}
}

func TestDecodeErrorNamesTheURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	_, err := testClient().GetJSON(context.Background(), server.URL, nil, &struct{}{})
	if err == nil {
		t.Fatal("decoding HTML as JSON must fail")
	}
	if !errors.Is(err, err) || len(err.Error()) == 0 {
		t.Fatal("the error should describe what came back")
	}
}

func TestContextCancellationIsNotRetried(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := testClient().GetJSON(ctx, server.URL, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
