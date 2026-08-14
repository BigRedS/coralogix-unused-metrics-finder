package coralogix

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, srvURL string, timeout time.Duration) *Client {
	t.Helper()
	c := NewClient("api.eu2.coralogix.com", "test-key", timeout)
	c.MetricsBase = srvURL
	return c
}

// A metric whose series query outlives the HTTP client timeout must surface as
// *SeriesTimeoutError so the scan can note it and move on.
func TestFetchSeriesForMetricClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, 50*time.Millisecond)
	_, _, err := c.FetchSeriesForMetric(context.Background(), "huge_metric", time.Now().Add(-time.Hour), time.Now(), 1000)

	var timeoutErr *SeriesTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("want *SeriesTimeoutError, got %T: %v", err, err)
	}
	if timeoutErr.MetricName != "huge_metric" {
		t.Errorf("MetricName = %q, want %q", timeoutErr.MetricName, "huge_metric")
	}
}

// An upstream gateway that gives up waiting on Coralogix is the same per-metric condition.
func TestFetchSeriesForMetricGatewayTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream took too long", http.StatusGatewayTimeout)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, time.Minute)
	_, _, err := c.FetchSeriesForMetric(context.Background(), "huge_metric", time.Now().Add(-time.Hour), time.Now(), 1000)

	var timeoutErr *SeriesTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("want *SeriesTimeoutError, got %T: %v", err, err)
	}
}

// Caller cancellation must stay a plain context error: the scan has to abort, not record
// every remaining metric as an unanalyzable timeout.
func TestFetchSeriesForMetricCallerDeadlineIsNotSeriesTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := c.FetchSeriesForMetric(ctx, "huge_metric", time.Now().Add(-time.Hour), time.Now(), 1000)

	var timeoutErr *SeriesTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("caller deadline was classified as a per-metric timeout: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

// A 500 must surface as *HTTPStatusError carrying the code, so callers can classify it without
// matching on message text — and it must keep the human-readable status + replication block.
func TestFetchSeriesForMetricServerErrorCarriesStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, time.Minute)
	_, _, err := c.FetchSeriesForMetric(context.Background(), "process_memory_usage_By", time.Now().Add(-time.Hour), time.Now(), 1000)

	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("want *HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want 500", statusErr.Code)
	}
	if !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Errorf("error text lost the status line: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP replication") {
		t.Errorf("error text lost the replication block: %v", err)
	}

	var timeoutErr *SeriesTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Errorf("500 classified as a timeout: %v", err)
	}
}

// Failures that are not metric-specific (auth, config) must not become skippable.
func TestFetchSeriesForMetricUnauthorizedIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, time.Minute)
	_, _, err := c.FetchSeriesForMetric(context.Background(), "some_metric", time.Now().Add(-time.Hour), time.Now(), 1000)
	if err == nil {
		t.Fatal("want error")
	}

	var timeoutErr *SeriesTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("401 classified as timeout: %v", err)
	}
	var limitErr *SeriesAnalysisLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("401 classified as series-analysis cap: %v", err)
	}
}
