package report

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Meta struct {
	APIHost                       string `json:"api_host"`
	SeriesLookbackSeconds         int    `json:"series_lookback_seconds"`
	SeriesLimitPerMetric          int    `json:"series_limit_per_metric"`
	DashboardsScanned             int    `json:"dashboards_scanned"`
	AlertsScanned                 int    `json:"alerts_scanned"`
	SLOsScanned                   int    `json:"slos_scanned"`
	DistinctMetricNames           int    `json:"distinct_metric_names"`
	DistinctSeriesInCatalog       int    `json:"distinct_series_in_catalog"`
	MetricsTruncatedAtSeriesLimit int    `json:"metrics_truncated_at_series_limit"`
	// CoralogixInternalMetricNamesSkipped counts metric names dropped before catalog fetch (prefix "cx_").
	// Those names are Coralogix platform metrics, not customer instrumentation.
	CoralogixInternalMetricNamesSkipped int    `json:"coralogix_internal_metric_names_skipped,omitempty"`
	UsageLookbackDays                   int    `json:"usage_lookback_days,omitempty"`           // Inclusive UTC calendar days in the CX billing request window (rolling or derived from calendar months).
	UsageBillingUTCStartDate            string `json:"usage_billing_utc_start_date,omitempty"`  // YYYY-MM-DD (CX Metrics Usage window).
	UsageBillingUTCEndDate              string `json:"usage_billing_utc_end_date,omitempty"`    // YYYY-MM-DD inclusive end date sent to CX.
	UsageBillingCalendarMonths          int    `json:"usage_billing_calendar_months,omitempty"` // >0 means window came from last N complete UTC months (not rolling days).
	SeriesWithBillingData               int    `json:"series_with_billing_data,omitempty"`
	UnusedSeriesWithBilling             int    `json:"unused_series_with_billing,omitempty"`
	// SeriesFetchFailuresCount counts metric names whose /api/v1/series query never returned a
	// catalog — rejected by the server-side per-query series-analysis cap, or timed out. Those
	// metrics are absent from the catalog and therefore from used/unused/OTEL outputs.
	SeriesFetchFailuresCount int `json:"series_fetch_failures_count,omitempty"`
	// SeriesFetchTimeoutsCount is the subset of SeriesFetchFailuresCount that timed out rather
	// than being refused outright. A higher --timeout-sec or shorter lookback may recover them.
	SeriesFetchTimeoutsCount int `json:"series_fetch_timeouts_count,omitempty"`
}

// Series fetch failure categories (MetricSeriesFetchFailure.Category).
const (
	// SeriesFailureAnalysisCap — Coralogix refused the query outright (ViolationTypeTotalSeriesAnalyzed).
	SeriesFailureAnalysisCap = "series_analysis_cap"
	// SeriesFailureTimeout — the query ran past the request timeout without answering.
	SeriesFailureTimeout = "timeout"
	// SeriesFailureServerError — Coralogix answered with a server error (typically 500) for this metric.
	SeriesFailureServerError = "server_error"
	// SeriesFailureTransport — the request failed before/outside an HTTP status (connection reset, bad body).
	SeriesFailureTransport = "transport"
)

// SeriesFetchFailureCounts tallies failures by category, e.g. {"timeout": 3, "server_error": 1}.
func SeriesFetchFailureCounts(failures []MetricSeriesFetchFailure) map[string]int {
	counts := make(map[string]int, 4)
	for _, f := range failures {
		cat := f.Category
		if cat == "" {
			cat = "unknown"
		}
		counts[cat]++
	}
	return counts
}

// SeriesFetchFailureBreakdown renders SeriesFetchFailureCounts as a stable, readable list
// ("2 timeout, 1 server_error") for warnings, CLI output and the PDF.
func SeriesFetchFailureBreakdown(failures []MetricSeriesFetchFailure) string {
	counts := SeriesFetchFailureCounts(failures)
	cats := make([]string, 0, len(counts))
	for cat := range counts {
		cats = append(cats, cat)
	}
	sort.Slice(cats, func(i, j int) bool {
		if counts[cats[i]] != counts[cats[j]] {
			return counts[cats[i]] > counts[cats[j]]
		}
		return cats[i] < cats[j]
	})
	parts := make([]string, 0, len(cats))
	for _, cat := range cats {
		parts = append(parts, strconv.Itoa(counts[cat])+" "+cat)
	}
	return strings.Join(parts, ", ")
}

// MetricSeriesFetchFailure records a metric whose catalog series fetch did not produce a result.
type MetricSeriesFetchFailure struct {
	MetricName string `json:"metric_name"`
	Category   string `json:"category,omitempty"`
	Reason     string `json:"reason"`
}

type DashboardRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type UsageCounts struct {
	Dashboards int `json:"dashboards"`
	Alerts     int `json:"alerts"`
	SLOs       int `json:"slos"`
	Total      int `json:"total"`
}

// SeriesBilling is Coralogix CX unit usage for a time series over the billing lookback window.
type SeriesBilling struct {
	UnitUsage   float64 `json:"unit_usage"`
	BytesVolume uint64  `json:"bytes_volume"`
	Cardinality uint64  `json:"cardinality"`
	SampleCount uint64  `json:"sample_count"`
	DaysInRange int     `json:"days_in_range"`
}

type UsedSeries struct {
	Series  string            `json:"series"`
	Labels  map[string]string `json:"labels"`
	Usage   UsageCounts       `json:"usage"`
	Billing *SeriesBilling    `json:"billing,omitempty"`
}

type UnusedSeries struct {
	Series  string            `json:"series"`
	Labels  map[string]string `json:"labels"`
	Billing *SeriesBilling    `json:"billing,omitempty"`
}

type SelectorRefIssue struct {
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id"`
	Selector   string `json:"selector"`
	MetricName string `json:"metric_name,omitempty"`
}

type Report struct {
	Meta                                               Meta               `json:"meta"`
	Dashboards                                         []DashboardRef     `json:"dashboards"`
	UsedSeriesInCatalog                                []UsedSeries       `json:"used_series_in_catalog"`
	UnusedSeriesInCatalog                              []UnusedSeries     `json:"unused_series_in_catalog"`
	ReferencedSelectorsWithoutMetricName               []SelectorRefIssue `json:"referenced_selectors_without_metric_name"`
	ReferencedSelectorsMetricAbsentInTimeseriesWindow  []SelectorRefIssue `json:"referenced_selectors_metric_absent_in_timeseries_window"`
	ReferencedSelectorsMetricPresentButNoSeriesMatches []SelectorRefIssue `json:"referenced_selectors_metric_present_but_no_series_matches"`
	// SeriesFetchFailures lists metric names whose catalog could not be fetched (server-side
	// series-analysis cap, or the query timing out). These metrics are excluded from used/unused
	// classification — their usage status is unknown rather than confirmed unused.
	SeriesFetchFailures []MetricSeriesFetchFailure `json:"series_fetch_failures,omitempty"`
	Warnings            []string                   `json:"warnings"`

	// BillingSplitCountBySeries records how many catalog series shared one billing variation row (>1 → usage was divided).
	BillingSplitCountBySeries map[string]int `json:"-"`
}

func SortUsedSeries(s []UsedSeries) {
	sort.Slice(s, func(i, j int) bool { return s[i].Series < s[j].Series })
}

func SortUnusedSeries(s []UnusedSeries) {
	sort.Slice(s, func(i, j int) bool { return s[i].Series < s[j].Series })
}

// SortUnusedByCost sorts unused series by unit_usage descending (highest cost first).
func SortUnusedByCost(s []UnusedSeries) {
	sort.Slice(s, func(i, j int) bool {
		ui, uj := unitUsageOf(s[i].Billing), unitUsageOf(s[j].Billing)
		if ui != uj {
			return ui > uj
		}
		return s[i].Series < s[j].Series
	})
}

func unitUsageOf(b *SeriesBilling) float64 {
	if b == nil {
		return 0
	}
	return b.UnitUsage
}

// UnusedByCostRow is a flat export row so unit_usage is visible without nesting (`billing` is omitted).
type UnusedByCostRow struct {
	Series         string            `json:"series"`
	MetricName     string            `json:"metric_name"`
	Labels         map[string]string `json:"labels"`
	BillingPresent bool              `json:"billing_present"`
	UnitUsage      float64           `json:"unit_usage"`
	BytesVolume    uint64            `json:"bytes_volume"`
	SampleCount    uint64            `json:"sample_count"`
	Cardinality    uint64            `json:"cardinality"`
	DaysInRange    int               `json:"days_in_range"`
	BillingSplitN  int               `json:"billing_split_n,omitempty"` // >1 when variation usage was split across N catalog series
}

func unusedRowsFromSeries(list []UnusedSeries, splitN map[string]int) []UnusedByCostRow {
	out := make([]UnusedByCostRow, 0, len(list))
	for _, u := range list {
		row := UnusedByCostRow{
			Series:     u.Series,
			Labels:     u.Labels,
			MetricName: u.Labels["__name__"],
		}
		if u.Billing != nil {
			row.BillingPresent = true
			row.UnitUsage = u.Billing.UnitUsage
			row.BytesVolume = u.Billing.BytesVolume
			row.SampleCount = u.Billing.SampleCount
			row.Cardinality = u.Billing.Cardinality
			row.DaysInRange = u.Billing.DaysInRange
		}
		if splitN != nil {
			if n := splitN[u.Series]; n > 1 {
				row.BillingSplitN = n
			}
		}
		out = append(out, row)
	}
	return out
}

// Write emits the report files to outputDir. If filenamePrefix is non-empty it is
// prepended (with a trailing "-") to every basename — e.g. "MyTeam-metric_usage_summary.json".
// Returns the list of written basenames so callers (CLI, webui) don't have to recompute them.
func (r *Report) Write(outputDir, filenamePrefix string) ([]string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}
	join := func(base string) (string, string) {
		name := base
		if filenamePrefix != "" {
			name = filenamePrefix + "-" + base
		}
		return name, outputDir + "/" + name
	}

	var written []string

	summaryName, summaryPath := join("metric_usage_summary.json")
	summary, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(summaryPath, summary, 0o644); err != nil {
		return nil, err
	}
	written = append(written, summaryName)

	unusedName, unusedPath := join("metric_usage_unused_series.json")
	unused, err := json.MarshalIndent(r.UnusedSeriesInCatalog, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(unusedPath, unused, 0o644); err != nil {
		return nil, err
	}
	written = append(written, unusedName)

	unusedByCost := make([]UnusedSeries, len(r.UnusedSeriesInCatalog))
	copy(unusedByCost, r.UnusedSeriesInCatalog)
	SortUnusedByCost(unusedByCost)

	splitN := r.BillingSplitCountBySeries
	if splitN == nil {
		splitN = map[string]int{}
	}
	costRows := unusedRowsFromSeries(unusedByCost, splitN)

	byCostName, byCostPath := join("metric_usage_unused_by_cost.json")
	byCost, err := json.MarshalIndent(costRows, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(byCostPath, byCost, 0o644); err != nil {
		return nil, err
	}
	written = append(written, byCostName)

	byCostCSVName, byCostCSVPath := join("metric_usage_unused_by_cost.csv")
	if err := WriteUnusedByCostCSV(byCostCSVPath, costRows); err != nil {
		return nil, err
	}
	written = append(written, byCostCSVName)

	byMetric := AggregateUnusedByMetric(costRows)
	byMetricName, byMetricPath := join("metric_usage_unused_by_metric.json")
	byMetricJSON, err := json.MarshalIndent(byMetric, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(byMetricPath, byMetricJSON, 0o644); err != nil {
		return nil, err
	}
	written = append(written, byMetricName)

	byMetricCSVName, byMetricCSVPath := join("metric_usage_unused_by_metric.csv")
	if err := WriteUnusedByMetricCSV(byMetricCSVPath, byMetric); err != nil {
		return nil, err
	}
	written = append(written, byMetricCSVName)

	allByMetric := AggregateAllByMetric(r.UsedSeriesInCatalog, r.UnusedSeriesInCatalog)
	allByMetricCSVName, allByMetricCSVPath := join("metric_usage_all_by_metric.csv")
	if err := WriteAllByMetricCSV(allByMetricCSVPath, allByMetric); err != nil {
		return nil, err
	}
	written = append(written, allByMetricCSVName)

	otelName, otelPath := join("metric_usage_otel_processors.yaml")
	if err := WriteOTELUnusedProcessorsYAML(otelPath, r.UsedSeriesInCatalog, r.UnusedSeriesInCatalog); err != nil {
		return nil, err
	}
	written = append(written, otelName)

	pdfName, pdfPath := join("metric_usage_report.pdf")
	plan := BuildOTELProcessorPlan(r.UsedSeriesInCatalog, r.UnusedSeriesInCatalog)
	if err := WritePDFReport(pdfPath, r, byMetric, plan, filenamePrefix); err != nil {
		return nil, err
	}
	written = append(written, pdfName)

	return written, nil
}
