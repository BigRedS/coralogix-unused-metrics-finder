package scan

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/report"
)

// newMetricsOnlyServer serves the two Prometheus endpoints the catalog phase needs. failFor maps
// a metric name to a handler that fails that metric's /api/v1/series query; all other metrics get
// one series back.
func newMetricsOnlyServer(t *testing.T, names []string, failFor map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/label/__name__/values", func(w http.ResponseWriter, r *http.Request) {
		quoted := make([]string, 0, len(names))
		for _, n := range names {
			quoted = append(quoted, `"`+n+`"`)
		}
		fmt.Fprintf(w, `{"status":"success","data":[%s]}`, strings.Join(quoted, ","))
	})
	mux.HandleFunc("/api/v1/series", func(w http.ResponseWriter, r *http.Request) {
		match := r.URL.Query().Get("match[]")
		for name, fail := range failFor {
			if strings.Contains(match, `"`+name+`"`) {
				fail(w, r)
				return
			}
		}
		for _, n := range names {
			if strings.Contains(match, `"`+n+`"`) {
				fmt.Fprintf(w, `{"status":"success","data":[{"__name__":"%s","job":"j"}]}`, n)
				return
			}
		}
		fmt.Fprint(w, `{"status":"success","data":[]}`)
	})
	return httptest.NewServer(mux)
}

func metricsOnlyOptions() Options {
	return Options{
		SeriesLookback:       time.Hour,
		SeriesLimitPerMetric: 1000,
		Workers:              2,
		Quiet:                true,
		SkipDashboards:       true,
		SkipAlerts:           true,
		SkipSLOs:             true,
	}
}

// A metric whose series query 500s must be recorded and skipped, leaving the rest of the scan intact.
func TestRunSkipsMetricWithServerError(t *testing.T) {
	names := []string{"good_one", "good_two", "boom_metric"}
	srv := newMetricsOnlyServer(t, names, map[string]http.HandlerFunc{
		"boom_metric": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal", http.StatusInternalServerError)
		},
	})
	defer srv.Close()

	client := coralogix.NewClient("api.eu2.coralogix.com", "k", time.Minute)
	client.MetricsBase = srv.URL

	rep, err := Run(context.Background(), client, metricsOnlyOptions())
	if err != nil {
		t.Fatalf("Run failed on a single per-metric 500: %v", err)
	}
	if len(rep.SeriesFetchFailures) != 1 {
		t.Fatalf("SeriesFetchFailures = %+v, want 1 entry", rep.SeriesFetchFailures)
	}
	f := rep.SeriesFetchFailures[0]
	if f.MetricName != "boom_metric" || f.Category != report.SeriesFailureServerError {
		t.Errorf("failure = %+v, want boom_metric/%s", f, report.SeriesFailureServerError)
	}
	if rep.Meta.DistinctSeriesInCatalog != 2 {
		t.Errorf("DistinctSeriesInCatalog = %d, want 2 (the metrics that did return)", rep.Meta.DistinctSeriesInCatalog)
	}
	if rep.Meta.SeriesFetchFailuresCount != 1 {
		t.Errorf("SeriesFetchFailuresCount = %d, want 1", rep.Meta.SeriesFetchFailuresCount)
	}
	if len(rep.UnusedSeriesInCatalog) != 2 {
		t.Errorf("unused series = %d, want 2 (no correlation sources scanned)", len(rep.UnusedSeriesInCatalog))
	}
	if !hasWarningContaining(rep.Warnings, "boom") && !hasWarningContaining(rep.Warnings, "series catalog could not be retrieved") {
		t.Errorf("warnings do not mention the skipped metric: %v", rep.Warnings)
	}
	// Billing is opt-in, and its absence must be stated rather than left to be inferred from
	// empty cost columns. This also proves the folding warning set reaches the report.
	if !hasWarningContaining(rep.Warnings, "CX billing data not collected") {
		t.Errorf("no warning that billing was not collected: %v", rep.Warnings)
	}
	if rep.HasBillingData() {
		t.Error("report claims billing data without a billing client")
	}
}

// When most of the catalog fails, the scan must refuse rather than report the remainder as unused.
func TestRunFailsWhenMostMetricsFail(t *testing.T) {
	names := []string{"a_metric", "b_metric", "c_metric"}
	failAll := map[string]http.HandlerFunc{}
	for _, n := range names {
		failAll[n] = func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal", http.StatusInternalServerError)
		}
	}
	srv := newMetricsOnlyServer(t, names, failAll)
	defer srv.Close()

	client := coralogix.NewClient("api.eu2.coralogix.com", "k", time.Minute)
	client.MetricsBase = srv.URL

	_, err := Run(context.Background(), client, metricsOnlyOptions())
	if err == nil {
		t.Fatal("Run succeeded with an empty catalog; want an error")
	}
	if !strings.Contains(err.Error(), "too much of the catalog is missing") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A 401 is a key problem, not a metric problem: it must fail the run immediately.
func TestRunFailsOnUnauthorizedSeriesQuery(t *testing.T) {
	names := []string{"a_metric", "b_metric"}
	srv := newMetricsOnlyServer(t, names, map[string]http.HandlerFunc{
		"a_metric": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad key", http.StatusUnauthorized)
		},
	})
	defer srv.Close()

	client := coralogix.NewClient("api.eu2.coralogix.com", "k", time.Minute)
	client.MetricsBase = srv.URL

	_, err := Run(context.Background(), client, metricsOnlyOptions())
	if err == nil {
		t.Fatal("Run succeeded despite a 401; want an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func hasWarningContaining(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
