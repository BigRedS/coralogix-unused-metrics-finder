package report

import "testing"

func TestSeriesFetchFailureBreakdown(t *testing.T) {
	failures := []MetricSeriesFetchFailure{
		{MetricName: "a", Category: SeriesFailureTimeout},
		{MetricName: "b", Category: SeriesFailureServerError},
		{MetricName: "c", Category: SeriesFailureTimeout},
		{MetricName: "d", Category: SeriesFailureAnalysisCap},
	}
	if got, want := SeriesFetchFailureBreakdown(failures), "2 timeout, 1 series_analysis_cap, 1 server_error"; got != want {
		t.Fatalf("breakdown = %q, want %q", got, want)
	}
	if got := SeriesFetchFailureBreakdown(nil); got != "" {
		t.Fatalf("breakdown of no failures = %q, want empty", got)
	}
}
