package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"shorter than limit", "0xabc", "0xabc"},
		{"exactly limit", strings.Repeat("a", shortIDLen), strings.Repeat("a", shortIDLen)},
		{"longer than limit", strings.Repeat("b", shortIDLen+10), strings.Repeat("b", shortIDLen)},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortID(tt.in); got != tt.want {
				t.Errorf("shortID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// testClient builds a Client whose URLs all point at the given test server and
// with no rate limiter (so retries are not throttled in tests).
func testClient(baseURL string) *Client {
	return NewClient(&Config{
		GammaBaseURL: baseURL,
		DataBaseURL:  baseURL,
		CLOBBaseURL:  baseURL,
		RateLimitRPS: 0,
	})
}

func TestGetJSONRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":42}`))
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	defer c.Close()

	var dest struct {
		Value int `json:"value"`
	}
	if err := c.getJSON(context.Background(), srv.URL, &dest); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if dest.Value != 42 {
		t.Errorf("decoded value = %d, want 42", dest.Value)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (500,500,200)", got)
	}
}

func TestGetJSONFailFastOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	defer c.Close()

	var dest map[string]any
	err := c.getJSON(context.Background(), srv.URL, &dest)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (no retry on 4xx)", got)
	}
}
