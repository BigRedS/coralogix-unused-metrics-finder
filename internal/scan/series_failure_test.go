package scan

import (
	"errors"
	"fmt"
	"testing"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/report"
)

func TestClassifySeriesFetchFailure(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantSkip bool
		wantCat  string
	}{
		{
			name:     "series analysis cap",
			err:      fmt.Errorf("wrapped: %w", &coralogix.SeriesAnalysisLimitError{MetricName: "m", Cause: errors.New("422 ViolationTypeTotalSeriesAnalyzed")}),
			wantSkip: true,
			wantCat:  report.SeriesFailureAnalysisCap,
		},
		{
			name:     "timeout",
			err:      fmt.Errorf("wrapped: %w", &coralogix.SeriesTimeoutError{MetricName: "m", Cause: errors.New("Client.Timeout exceeded while awaiting headers\n\ncurl ...")}),
			wantSkip: true,
			wantCat:  report.SeriesFailureTimeout,
		},
		{
			name:     "server error on one metric",
			err:      fmt.Errorf("wrapped: %w", &coralogix.HTTPStatusError{Code: 500, Status: "500 Internal Server Error", Detail: "curl ..."}),
			wantSkip: true,
			wantCat:  report.SeriesFailureServerError,
		},
		{
			name:     "rate limited",
			err:      &coralogix.HTTPStatusError{Code: 429, Status: "429 Too Many Requests"},
			wantSkip: true,
			wantCat:  report.SeriesFailureServerError,
		},
		{
			name:     "transport failure",
			err:      errors.New("read tcp 10.0.0.1:443: connection reset by peer\n\ncurl ..."),
			wantSkip: true,
			wantCat:  report.SeriesFailureTransport,
		},
		{
			name:     "unauthorized is fatal",
			err:      fmt.Errorf("wrapped: %w", &coralogix.HTTPStatusError{Code: 401, Status: "401 Unauthorized"}),
			wantSkip: false,
		},
		{
			name:     "forbidden is fatal",
			err:      &coralogix.HTTPStatusError{Code: 403, Status: "403 Forbidden"},
			wantSkip: false,
		},
		{
			name:     "not found is fatal",
			err:      &coralogix.HTTPStatusError{Code: 404, Status: "404 Not Found"},
			wantSkip: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, skip := classifySeriesFetchFailure("my_metric", tc.err)
			if skip != tc.wantSkip {
				t.Fatalf("skip = %v, want %v", skip, tc.wantSkip)
			}
			if !skip {
				return
			}
			if got.Category != tc.wantCat {
				t.Errorf("Category = %q, want %q", got.Category, tc.wantCat)
			}
			if got.MetricName != "my_metric" {
				t.Errorf("MetricName = %q, want %q", got.MetricName, "my_metric")
			}
			if got.Reason == "" {
				t.Error("Reason is empty")
			}
		})
	}
}

// The multi-line curl replication block in client errors must not leak into report Reasons.
func TestFirstLineTrimsDiagnosticBlock(t *testing.T) {
	err := errors.New("context deadline exceeded (Client.Timeout exceeded)\n\ncurl -X GET 'https://...' -H 'Authorization: Bearer ...'")
	got := firstLine(err)
	if got != "context deadline exceeded (Client.Timeout exceeded)" {
		t.Fatalf("firstLine = %q", got)
	}
}
