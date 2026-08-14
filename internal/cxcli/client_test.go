package cxcli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/promqlextract"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/scan"
)

// stubRunner records the args of each call and returns canned responses keyed by
// the joined subcommand (before the appended profile/output flags).
type stubRunner struct {
	responses map[string][]byte
	errs      map[string]error
	calls     [][]string
}

func (s *stubRunner) run(_ context.Context, args ...string) ([]byte, error) {
	s.calls = append(s.calls, args)
	key := strings.Join(args, " ")
	if err, ok := s.errs[key]; ok {
		return nil, err
	}
	for prefix, body := range s.responses {
		if strings.HasPrefix(key, prefix) {
			return body, nil
		}
	}
	return nil, errors.New("no stub for: " + key)
}

func newTestClient(s *stubRunner, opts ...Option) *Client {
	return NewClient("test-profile", append([]Option{WithRunner(s.run)}, opts...)...)
}

func TestFetchMetricNames(t *testing.T) {
	s := &stubRunner{responses: map[string][]byte{
		"metrics search --name *": []byte(`["a_total","b_seconds","up"]`),
	}}
	c := newTestClient(s)

	got, err := c.FetchMetricNames(context.Background(), time.Now(), time.Now())
	if err != nil {
		t.Fatalf("FetchMetricNames: %v", err)
	}
	want := []string{"a_total", "b_seconds", "up"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if len(s.calls) != 1 || strings.Join(s.calls[0], " ") != "metrics search --name *" {
		t.Fatalf("unexpected call args: %v", s.calls)
	}
}

func TestFetchDashboardCatalog(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []coralogix.DashboardCatalogItem
	}{
		{
			name: "array",
			body: `[{"id":"d1","name":"One"},{"id":"d2","name":"Two"}]`,
			want: []coralogix.DashboardCatalogItem{{ID: "d1", Name: "One"}, {ID: "d2", Name: "Two"}},
		},
		{
			name: "object envelope",
			body: `{"items":[{"id":"d3","name":"Three"}]}`,
			want: []coralogix.DashboardCatalogItem{{ID: "d3", Name: "Three"}},
		},
		{
			name: "empty (broken catalog)",
			body: `[]`,
			want: []coralogix.DashboardCatalogItem{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stubRunner{responses: map[string][]byte{"dashboards catalog": []byte(tt.body)}}
			c := newTestClient(s)
			got, err := c.FetchDashboardCatalog(context.Background())
			if err != nil {
				t.Fatalf("FetchDashboardCatalog: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFetchAllAlertDefs_WrapsIDAndKeepsPromQL(t *testing.T) {
	s := &stubRunner{responses: map[string][]byte{
		"alerts list": []byte(`[{"id":"al-1","name":"A"},{"id":"al-2","name":"B"}]`),
		"alerts get al-1": []byte(`{"alertDefProperties":{"metricThreshold":` +
			`{"metricFilter":{"promql":"http_requests_total{job=\"web\"}"}}}}`),
		"alerts get al-2": []byte(`{"alertDefProperties":{"metricThreshold":` +
			`{"metricFilter":{"promql":"up{env=\"prod\"}"}}}}`),
	}}
	c := newTestClient(s)

	defs, err := c.FetchAllAlertDefs(context.Background(), 200)
	if err != nil {
		t.Fatalf("FetchAllAlertDefs: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d defs, want 2", len(defs))
	}

	// coralogix.AlertID must recover the injected id...
	if id := coralogix.AlertID(defs[0]); id != "al-1" {
		t.Fatalf("AlertID(defs[0]) = %q, want al-1", id)
	}
	// ...and the nested PromQL must still be discoverable.
	sels := promqlextract.ExtractFromJSON(defs[0])
	if len(sels) == 0 {
		t.Fatalf("expected PromQL selectors extracted from wrapped def, got none")
	}
	foundMetric := false
	for _, vs := range sels {
		if promqlextract.MetricName(vs.Selector) == "http_requests_total" {
			foundMetric = true
		}
	}
	if !foundMetric {
		t.Fatalf("did not extract http_requests_total from %s", string(defs[0]))
	}
}

func TestFetchSLOs_Empty(t *testing.T) {
	s := &stubRunner{responses: map[string][]byte{"slos list": []byte(`[]`)}}
	c := newTestClient(s)
	defs, err := c.FetchSLOs(context.Background())
	if err != nil {
		t.Fatalf("FetchSLOs: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("got %d defs, want 0", len(defs))
	}
}

func TestFetchSeriesForMetric_UnionsLabels(t *testing.T) {
	body := `[
		{"metric":{"__name__":"up","job":"web","instance":"i-1"},"values":[[1,"1"]]},
		{"metric":{"__name__":"up","job":"web","instance":"i-2"},"values":[[1,"1"]]}
	]`
	s := &stubRunner{responses: map[string][]byte{`metrics query-range`: []byte(body)}}
	c := newTestClient(s)

	series, truncated, err := c.FetchSeriesForMetric(context.Background(), "up", time.Now().Add(-time.Hour), time.Now(), 0)
	if err != nil {
		t.Fatalf("FetchSeriesForMetric: %v", err)
	}
	if truncated {
		t.Fatalf("truncated should always be false in CLI mode")
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}
	// Confirm the selector arg was built correctly.
	got := strings.Join(s.calls[0], " ")
	if !strings.Contains(got, `metrics query-range {__name__="up"}`) {
		t.Fatalf("unexpected query-range args: %v", s.calls[0])
	}
}

func TestFetchSeriesForMetric_ErrorBecomesAnalysisLimit(t *testing.T) {
	c := newTestClient(&stubRunner{})
	c.run = func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("server exploded")
	}

	_, _, err := c.FetchSeriesForMetric(context.Background(), "huge", time.Now().Add(-time.Hour), time.Now(), 0)
	if err == nil {
		t.Fatal("expected error")
	}
	var limitErr *coralogix.SeriesAnalysisLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *SeriesAnalysisLimitError, got %T: %v", err, err)
	}
	if limitErr.MetricName != "huge" {
		t.Fatalf("MetricName = %q, want huge", limitErr.MetricName)
	}
}

func TestFetchSeriesForMetric_ContextCancelPropagates(t *testing.T) {
	c := newTestClient(&stubRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.run = func(ctx context.Context, _ ...string) ([]byte, error) {
		return nil, ctx.Err()
	}
	_, _, err := c.FetchSeriesForMetric(ctx, "m", time.Now(), time.Now(), 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestTimeoutDefaultAndOverride(t *testing.T) {
	if got := NewClient("p").timeout; got != defaultTimeout {
		t.Fatalf("default timeout = %v, want %v", got, defaultTimeout)
	}
	if got := NewClient("p", WithTimeout(5*time.Second)).timeout; got != 5*time.Second {
		t.Fatalf("override timeout = %v, want 5s", got)
	}
	// Zero/negative is ignored, keeping the default.
	if got := NewClient("p", WithTimeout(0)).timeout; got != defaultTimeout {
		t.Fatalf("zero timeout = %v, want default %v", got, defaultTimeout)
	}
}

func TestHostDefaultAndOverride(t *testing.T) {
	if h := NewClient("p").Host(); h != "cx-profile:p" {
		t.Fatalf("default host = %q, want cx-profile:p", h)
	}
	if h := NewClient("p", WithHost("api.eu2.coralogix.com")).Host(); h != "api.eu2.coralogix.com" {
		t.Fatalf("override host = %q", h)
	}
}

func TestExecArgsAppendProfileFlags(t *testing.T) {
	// Ensure the default exec path would append the expected global flags by
	// inspecting via a runner stand-in is not possible; instead verify stepArg.
	if got := stepArg(time.Hour); got != "3600s" {
		t.Fatalf("stepArg(1h) = %q, want 3600s", got)
	}
	if got := stepArg(0); got != "1s" {
		t.Fatalf("stepArg(0) = %q, want 1s", got)
	}
}

// Compile-time guarantee that *Client satisfies scan.Source.
var _ scan.Source = (*Client)(nil)

// flakyRunner fails the first failures[key] calls for a given subcommand with
// err, then falls through to responses. It models the transient API 500s that
// `cx alerts get` returns on large tenants.
type flakyRunner struct {
	responses map[string][]byte
	failures  map[string]int
	err       error
	calls     map[string]int
}

func (f *flakyRunner) run(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[key]++
	if f.failures[key] > 0 {
		f.failures[key]--
		return nil, f.err
	}
	for prefix, body := range f.responses {
		if strings.HasPrefix(key, prefix) {
			return body, nil
		}
	}
	return nil, errors.New("no stub for: " + key)
}

const transientErr = `cx alerts get al-2: exit status 1: API request failed (500): Internal Server Error: Internal error`

// A definition that 500s must be retried, not skipped: dropping it would hide
// every metric that alert uses and report those metrics as unused.
func TestFetchAllAlertDefs_RetriesTransientFailure(t *testing.T) {
	f := &flakyRunner{
		responses: map[string][]byte{
			"alerts list":     []byte(`[{"id":"al-1"},{"id":"al-2"}]`),
			"alerts get al-1": []byte(`{"a":1}`),
			"alerts get al-2": []byte(`{"b":2}`),
		},
		failures: map[string]int{"alerts get al-2": 2},
		err:      errors.New(transientErr),
	}
	c := NewClient("p", WithRunner(f.run), WithRetry(4, 0))

	defs, err := c.FetchAllAlertDefs(context.Background(), 200)
	if err != nil {
		t.Fatalf("FetchAllAlertDefs after transient failures: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d defs, want 2 — a retried alert must not be dropped", len(defs))
	}
	if got := f.calls["alerts get al-2"]; got != 3 {
		t.Fatalf("alerts get al-2 called %d times, want 3 (2 failures + 1 success)", got)
	}
	if id := coralogix.AlertID(defs[1]); id != "al-2" {
		t.Fatalf("AlertID(defs[1]) = %q, want al-2", id)
	}
}

// If retries are exhausted the scan must abort. Returning the alerts that did
// succeed would silently under-report usage, which is the failure mode the
// whole tool exists to avoid.
func TestFetchAllAlertDefs_AbortsWhenRetriesExhausted(t *testing.T) {
	f := &flakyRunner{
		responses: map[string][]byte{
			"alerts list":     []byte(`[{"id":"al-1"},{"id":"al-2"}]`),
			"alerts get al-1": []byte(`{"a":1}`),
		},
		failures: map[string]int{"alerts get al-2": 99},
		err:      errors.New(transientErr),
	}
	c := NewClient("p", WithRunner(f.run), WithRetry(3, 0))

	defs, err := c.FetchAllAlertDefs(context.Background(), 200)
	if err == nil {
		t.Fatal("expected an error once retries are exhausted, got nil")
	}
	if defs != nil {
		t.Fatalf("expected no partial definitions on abort, got %d", len(defs))
	}
	if !strings.Contains(err.Error(), "al-2") {
		t.Fatalf("error should name the failing alert, got %q", err)
	}
	if got := f.calls["alerts get al-2"]; got != 3 {
		t.Fatalf("alerts get al-2 called %d times, want 3 attempts", got)
	}
}

// A misconfigured profile is not worth four backoffs — fail on the first answer.
func TestRunRetrying_FailsFastOnPermanentError(t *testing.T) {
	f := &flakyRunner{
		responses: map[string][]byte{"alerts list": []byte(`[]`)},
		failures:  map[string]int{"alerts list": 99},
		err:       errors.New("cx alerts list: exit status 1: Configuration error: Profile 'nope' not found"),
	}
	c := NewClient("nope", WithRunner(f.run), WithRetry(4, time.Minute))

	if _, err := c.FetchAllAlertDefs(context.Background(), 200); err == nil {
		t.Fatal("expected an error for a missing profile")
	}
	if got := f.calls["alerts list"]; got != 1 {
		t.Fatalf("alerts list called %d times, want 1 (no retries on a config error)", got)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"API request failed (500): Internal Server Error", true},
		{"API request failed (503): Service Unavailable", true},
		{"context deadline exceeded", true},
		{"Configuration error: Profile 'x' not found", false},
		{"API request failed (401): Unauthorized", false},
		{"API request failed (404): Not Found", false},
	}
	for _, tc := range cases {
		if got := isRetryable(errors.New(tc.msg)); got != tc.want {
			t.Errorf("isRetryable(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// A cancelled scan must not sit through the remaining backoff.
func TestRunRetrying_HonoursContextCancellation(t *testing.T) {
	f := &flakyRunner{
		failures: map[string]int{"alerts list": 99},
		err:      errors.New(transientErr),
	}
	c := NewClient("p", WithRunner(f.run), WithRetry(5, time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.runRetrying(ctx, "alerts", "list")
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runRetrying did not return promptly after cancellation")
	}
}
