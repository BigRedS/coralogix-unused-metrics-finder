package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/metricusage"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/promqlextract"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/report"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/status"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/strintern"
	"golang.org/x/sync/errgroup"
)

type Options struct {
	SeriesLookback       time.Duration
	SeriesLimitPerMetric int
	Workers              int
	// UsageLookbackDays is an inclusive rolling UTC calendar-day window for CX unit_usage when UsageBillingCalendarMonths is 0 (0 means “do not use rolling days” unless calendar months set).
	UsageLookbackDays int
	// UsageBillingCalendarMonths if >0 selects the last N complete UTC calendar months for CX billing (exclusive of the current partial month). Overrides the rolling day window when both are set.
	UsageBillingCalendarMonths int
	// Billing queries Metrics Usage API; nil skips billing even if UsageLookbackDays > 0.
	Billing *metricusage.Client
	// Status receives progress updates; nil uses a stderr status line.
	Status io.Writer
	// Quiet disables the status line entirely.
	Quiet bool
	// SkipDashboards skips dashboard catalog and definitions (no dashboard PromQL correlation).
	SkipDashboards bool
	// SkipAlerts skips alert definitions v3 (no alert PromQL correlation).
	SkipAlerts bool
	// SkipSLOs skips SLO list (no SLO PromQL correlation).
	SkipSLOs bool
}

type resourceRef struct {
	selectors []promqlextract.VectorSelector
}

// billingWindowUTC returns inclusive UTC calendar dates (midnight-aligned) for Metrics Usage API CommonRequestFields.
// If calendarMonths > 0, the window is [first day of month N months before current month, last day of previous month].
// Otherwise lookbackDays must be > 0 and the window is [today−(lookbackDays−1), today] in UTC.
func billingWindowUTC(now time.Time, lookbackDays, calendarMonths int) (startDay, endDay time.Time, inclusiveDays int, err error) {
	now = now.UTC()
	if calendarMonths > 0 {
		if calendarMonths > 240 {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("usage billing calendar months %d exceeds max 240", calendarMonths)
		}
		firstThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		endDay = firstThisMonth.AddDate(0, 0, -1)
		startDay = firstThisMonth.AddDate(0, -calendarMonths, 0)
		inclusiveDays = int(endDay.Sub(startDay).Hours()/24) + 1
		return startDay, endDay, inclusiveDays, nil
	}
	if lookbackDays <= 0 {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("usage lookback days must be positive when usage billing calendar months is 0")
	}
	endDay = now.Truncate(24 * time.Hour)
	startDay = endDay.AddDate(0, 0, -(lookbackDays - 1))
	return startDay, endDay, lookbackDays, nil
}

func Run(ctx context.Context, client *coralogix.Client, opt Options) (*report.Report, error) {
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	if opt.SeriesLookback <= 0 {
		opt.SeriesLookback = 25 * time.Hour
	}
	if opt.SeriesLimitPerMetric <= 0 {
		opt.SeriesLimitPerMetric = 50_000
	}

	now := time.Now()
	start := now.Add(-opt.SeriesLookback)

	var st *status.Line
	if !opt.Quiet {
		w := opt.Status
		if w == nil {
			w = os.Stderr
		}
		st = status.New(w)
		defer st.Done("")
	}

	setStatus := func(msg string) {
		if st != nil {
			st.Set(msg)
		}
	}

	var warnings []string
	resources := make(map[string]map[string]resourceRef)

	var dashboards []report.DashboardRef
	resources["dashboard"] = make(map[string]resourceRef)
	if opt.SkipDashboards {
		warnings = append(warnings, "skipped dashboards (--skip-dashboards); correlation excludes dashboard PromQL")
	} else {
		setStatus("listing dashboard catalog…")
		catalog, err := client.FetchDashboardCatalog(ctx)
		if err != nil {
			return nil, fmt.Errorf("dashboard catalog: %w", err)
		}
		dashboards = make([]report.DashboardRef, 0, len(catalog))
		dashTotal := len(catalog)
		dashDone := 0
		for _, item := range catalog {
			if item.ID == "" {
				continue
			}
			dashDone++
			setStatus(fmt.Sprintf("dashboards %d/%d — %s", dashDone, dashTotal, truncate(item.Name, 50)))
			raw, err := client.FetchDashboard(ctx, item.ID)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("dashboard %s: %v", item.ID, err))
				continue
			}
			resources["dashboard"][item.ID] = resourceRef{selectors: promqlextract.ExtractFromJSON(raw)}
			dashboards = append(dashboards, report.DashboardRef{ID: item.ID, Name: item.Name})
		}
	}

	var alerts []json.RawMessage
	resources["alert"] = make(map[string]resourceRef)
	if opt.SkipAlerts {
		warnings = append(warnings, "skipped alerts (--skip-alerts); correlation excludes alert PromQL")
	} else {
		setStatus("fetching alert definitions…")
		var err error
		alerts, err = client.FetchAllAlertDefs(ctx, 200)
		if err != nil {
			return nil, fmt.Errorf("alerts: %w", err)
		}
		for _, raw := range alerts {
			id := coralogix.AlertID(raw)
			if id == "" {
				continue
			}
			resources["alert"][id] = resourceRef{selectors: promqlextract.ExtractFromJSON(raw)}
		}
		setStatus(fmt.Sprintf("parsed %d alerts", len(alerts)))
	}

	var slos []json.RawMessage
	resources["slo"] = make(map[string]resourceRef)
	if opt.SkipSLOs {
		warnings = append(warnings, "skipped SLOs (--skip-slo); correlation excludes SLO PromQL")
	} else {
		setStatus("fetching SLOs…")
		var err error
		slos, err = client.FetchSLOs(ctx)
		if err != nil {
			return nil, fmt.Errorf("slos: %w", err)
		}
		for _, raw := range slos {
			id := coralogix.SLOID(raw)
			if id == "" {
				continue
			}
			resources["slo"][id] = resourceRef{selectors: promqlextract.ExtractFromJSON(raw)}
		}
		setStatus(fmt.Sprintf("parsed %d SLOs", len(slos)))
	}

	if opt.SkipDashboards && opt.SkipAlerts && opt.SkipSLOs {
		warnings = append(warnings, "all correlation sources skipped; every catalog series will appear unused")
	}

	// Metric catalog (parallel series fetch per metric name)
	setStatus("listing metric names…")
	metricNames, err := client.FetchMetricNames(ctx, start, now)
	if err != nil {
		return nil, fmt.Errorf("metric names: %w", err)
	}
	metricNames, cxMetricNamesSkipped := omitCoralogixInternalMetricNames(metricNames)

	var mu sync.Mutex
	catalogSeries := make(map[string]map[string]string)
	// Label names and values repeat across nearly every series; json decoding allocates a fresh
	// copy of each per series. Interning them is what keeps a multi-million-series catalog inside
	// available memory — without it the retained label strings dominate the scan's heap.
	labels := strintern.New()
	truncatedCount := 0
	var seriesFetchFailures []report.MetricSeriesFetchFailure
	metricsTotal := int64(len(metricNames))
	var metricsDone atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, opt.Workers)

	setStatus(fmt.Sprintf("metrics 0/%d — fetching series…", metricsTotal))

	for _, mname := range metricNames {
		mname := mname
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				return gctx.Err()
			}
			defer func() { <-sem }()

			series, trunc, err := client.FetchSeriesForMetric(gctx, mname, start, now, opt.SeriesLimitPerMetric)
			if err != nil {
				// A cancelled/expired caller context is scan-wide: stop rather than blaming the metric.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failure, skip := classifySeriesFetchFailure(mname, err)
				if !skip {
					return err
				}
				mu.Lock()
				seriesFetchFailures = append(seriesFetchFailures, failure)
				mu.Unlock()
				done := metricsDone.Add(1)
				setStatus(fmt.Sprintf(
					"metrics %d/%d — %s on %s (skipped)",
					done, metricsTotal, failure.Category, truncate(mname, 40),
				))
				return nil
			}
			// Intern outside the catalog lock: the table has its own (sharded) locking, and
			// this is the expensive part of absorbing a metric's series.
			for i, lbls := range series {
				series[i] = labels.Labels(lbls)
			}

			mu.Lock()
			if trunc {
				truncatedCount++
			}
			for _, lbls := range series {
				key := promqlextract.CanonicalSeries(lbls)
				catalogSeries[key] = lbls
			}
			seriesCount := len(catalogSeries)
			mu.Unlock()

			done := metricsDone.Add(1)
			setStatus(fmt.Sprintf(
				"metrics %d/%d — %d series — %s",
				done, metricsTotal, seriesCount, truncate(mname, 40),
			))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// The catalog now holds the only references it needs; the intern table's own entries (~50
	// bytes per distinct string) are pure overhead from here on. Record its size and drop it so
	// the GC can reclaim the table before the memory-hungry correlation and output phases.
	labelStringsRetained := labels.Len()
	labels = nil
	_ = labels // released deliberately: see above

	// Failures arrive from parallel workers; sort so report output is stable across runs.
	sort.Slice(seriesFetchFailures, func(i, j int) bool {
		return seriesFetchFailures[i].MetricName < seriesFetchFailures[j].MetricName
	})

	seriesFetchTimeouts := report.SeriesFetchFailureCounts(seriesFetchFailures)[report.SeriesFailureTimeout]
	if len(seriesFetchFailures) > 0 {
		// Too much of the catalog missing means the remainder can't support "unused" claims.
		if float64(len(seriesFetchFailures)) > maxSeriesFetchFailureFraction*float64(len(metricNames)) {
			return nil, fmt.Errorf(
				"series fetch failed for %d of %d metric names (%s) — too much of the catalog is missing to report on; first failure: %s",
				len(seriesFetchFailures), len(metricNames),
				report.SeriesFetchFailureBreakdown(seriesFetchFailures),
				seriesFetchFailures[0].MetricName,
			)
		}
		warnings = append(warnings, fmt.Sprintf(
			"%d of %d metric name(s) skipped because their series catalog could not be retrieved (%s) — see series_fetch_failures for the list and per-metric reasons. These metrics are excluded from used/unused classification and the OTEL fragment; their usage status is unknown, not unused.",
			len(seriesFetchFailures), len(metricNames), report.SeriesFetchFailureBreakdown(seriesFetchFailures),
		))
	}

	setStatus("correlating selectors with catalog…")

	seriesByMetric := make(map[string][]struct {
		key    string
		labels map[string]string
	})
	for key, lbls := range catalogSeries {
		mn := lbls["__name__"]
		seriesByMetric[mn] = append(seriesByMetric[mn], struct {
			key    string
			labels map[string]string
		}{key: key, labels: lbls})
	}

	type usage struct {
		dashboards map[string]struct{}
		alerts     map[string]struct{}
		slos       map[string]struct{}
	}
	usageBySeries := make(map[string]*usage)

	var (
		noMetricName []report.SelectorRefIssue
		metricAbsent []report.SelectorRefIssue
		noLabelMatch []report.SelectorRefIssue
	)

	addUsage := func(seriesKey, kind, rid string) {
		u, ok := usageBySeries[seriesKey]
		if !ok {
			u = &usage{
				dashboards: make(map[string]struct{}),
				alerts:     make(map[string]struct{}),
				slos:       make(map[string]struct{}),
			}
			usageBySeries[seriesKey] = u
		}
		switch kind {
		case "dashboard":
			u.dashboards[rid] = struct{}{}
		case "alert":
			u.alerts[rid] = struct{}{}
		case "slo":
			u.slos[rid] = struct{}{}
		}
	}

	for kind, byID := range resources {
		for rid, ref := range byID {
			for _, vs := range ref.selectors {
				mname := promqlextract.MetricName(vs.Selector)
				if mname == "" {
					noMetricName = append(noMetricName, report.SelectorRefIssue{
						Kind: kind, ResourceID: rid, Selector: vs.Canonical,
					})
					continue
				}
				if promqlextract.IsCoralogixInternalMetricName(mname) {
					continue
				}
				candidates := seriesByMetric[mname]
				if len(candidates) == 0 {
					metricAbsent = append(metricAbsent, report.SelectorRefIssue{
						Kind: kind, ResourceID: rid, Selector: vs.Canonical, MetricName: mname,
					})
					continue
				}
				matched := false
				for _, c := range candidates {
					if promqlextract.MatchesSeries(vs.Selector, c.labels) {
						matched = true
						addUsage(c.key, kind, rid)
					}
				}
				if !matched {
					noLabelMatch = append(noLabelMatch, report.SelectorRefIssue{
						Kind: kind, ResourceID: rid, Selector: vs.Canonical, MetricName: mname,
					})
				}
			}
		}
	}

	billingBySeries := map[string]report.SeriesBilling{}
	billingSplitBySeries := map[string]int{}
	billingStartStr := ""
	billingEndStr := ""
	billingInclusiveDays := opt.UsageLookbackDays
	billingCalMonths := opt.UsageBillingCalendarMonths

	billingEnabled := opt.Billing != nil && (opt.UsageLookbackDays > 0 || opt.UsageBillingCalendarMonths > 0)
	if billingEnabled {
		startDay, endDay, inclDays, winErr := billingWindowUTC(time.Now(), opt.UsageLookbackDays, opt.UsageBillingCalendarMonths)
		if winErr != nil {
			warnings = append(warnings, "billing window: "+winErr.Error())
		} else {
			billingStartStr = startDay.Format("2006-01-02")
			billingEndStr = endDay.Format("2006-01-02")
			billingInclusiveDays = inclDays
			if opt.UsageBillingCalendarMonths > 0 {
				setStatus(fmt.Sprintf("fetching CX unit usage (%d complete UTC months ≈ %d inclusive days)…", opt.UsageBillingCalendarMonths, billingInclusiveDays))
			} else {
				setStatus(fmt.Sprintf("fetching CX unit usage (%d UTC days)…", billingInclusiveDays))
			}
			raw, splitCounts, perMetricWarnings, err := opt.Billing.EnrichCatalog(ctx, catalogSeries, metricNames, startDay, endDay, opt.Workers, func(done, total int, metric string) {
				setStatus(fmt.Sprintf("billing %d/%d — %s", done, total, truncate(metric, 40)))
			})
			for _, w := range perMetricWarnings {
				warnings = append(warnings, "billing units: "+w)
			}
			if err != nil {
				warnings = append(warnings, "billing units: "+err.Error())
			} else {
				for k, u := range raw {
					billingBySeries[k] = toReportBilling(u)
				}
				billingSplitBySeries = splitCounts
			}
		}
	}

	used := make([]report.UsedSeries, 0)
	for key, lbls := range catalogSeries {
		u, ok := usageBySeries[key]
		if !ok || (len(u.dashboards)+len(u.alerts)+len(u.slos) == 0) {
			continue
		}
		used = append(used, report.UsedSeries{
			Series: key,
			Labels: lbls,
			Usage: report.UsageCounts{
				Dashboards: len(u.dashboards),
				Alerts:     len(u.alerts),
				SLOs:       len(u.slos),
				Total:      len(u.dashboards) + len(u.alerts) + len(u.slos),
			},
			Billing: billingPtr(billingBySeries, key),
		})
	}
	report.SortUsedSeries(used)

	unused := make([]report.UnusedSeries, 0)
	for key, lbls := range catalogSeries {
		u, ok := usageBySeries[key]
		if ok && (len(u.dashboards)+len(u.alerts)+len(u.slos) > 0) {
			continue
		}
		unused = append(unused, report.UnusedSeries{
			Series:  key,
			Labels:  lbls,
			Billing: billingPtr(billingBySeries, key),
		})
	}
	report.SortUnusedSeries(unused)

	unusedWithBilling := 0
	for _, u := range unused {
		if u.Billing != nil {
			unusedWithBilling++
		}
	}

	setStatus(fmt.Sprintf(
		"done — %d series (%d used, %d unused)",
		len(catalogSeries), len(used), len(unused),
	))

	return &report.Report{
		Meta: report.Meta{
			APIHost:                             client.APIHost,
			SeriesLookbackSeconds:               int(opt.SeriesLookback.Seconds()),
			SeriesLimitPerMetric:                opt.SeriesLimitPerMetric,
			DashboardsScanned:                   len(dashboards),
			AlertsScanned:                       len(alerts),
			SLOsScanned:                         len(slos),
			DistinctMetricNames:                 len(metricNames),
			DistinctSeriesInCatalog:             len(catalogSeries),
			MetricsTruncatedAtSeriesLimit:       truncatedCount,
			CoralogixInternalMetricNamesSkipped: cxMetricNamesSkipped,
			UsageLookbackDays:                   billingInclusiveDays,
			UsageBillingUTCStartDate:            billingStartStr,
			UsageBillingUTCEndDate:              billingEndStr,
			UsageBillingCalendarMonths:          billingCalMonths,
			SeriesWithBillingData:               len(billingBySeries),
			UnusedSeriesWithBilling:             unusedWithBilling,
			SeriesFetchFailuresCount:            len(seriesFetchFailures),
			SeriesFetchTimeoutsCount:            seriesFetchTimeouts,
			DistinctLabelStringsRetained:        labelStringsRetained,
		},
		Dashboards:                           dashboards,
		UsedSeriesInCatalog:                  used,
		UnusedSeriesInCatalog:                unused,
		ReferencedSelectorsWithoutMetricName: noMetricName,
		ReferencedSelectorsMetricAbsentInTimeseriesWindow:  metricAbsent,
		ReferencedSelectorsMetricPresentButNoSeriesMatches: noLabelMatch,
		SeriesFetchFailures:       seriesFetchFailures,
		Warnings:                  warnings,
		BillingSplitCountBySeries: billingSplitBySeries,
	}, nil
}

// maxSeriesFetchFailureFraction bounds how much of the catalog may be missing before the scan
// refuses to report at all. Individual metrics failing is normal (huge metrics break the series
// endpoint); most of them failing means something systemic — a rate limit, an outage — and a
// report built from the remainder would call metrics unused on no evidence.
const maxSeriesFetchFailureFraction = 0.5

// classifySeriesFetchFailure decides whether a /api/v1/series failure for one metric can be
// noted and skipped (skip=true) or must fail the whole scan. Skipping is the default: the
// endpoint fails in several metric-specific ways (the series-analysis cap, timeouts, and 5xx
// on metrics it cannot enumerate), and one bad metric must not throw away the scan. Only
// statuses that say the request itself was unacceptable — bad key, missing permission, bad
// path — abort, since those would fail identically for every metric and would otherwise
// produce a report claiming the whole estate is unused. Run-wide safety net: the caller also
// aborts if too large a fraction of metrics failed (see maxSeriesFetchFailureFraction).
func classifySeriesFetchFailure(metricName string, err error) (report.MetricSeriesFetchFailure, bool) {
	failure := report.MetricSeriesFetchFailure{MetricName: metricName}

	var limitErr *coralogix.SeriesAnalysisLimitError
	if errors.As(err, &limitErr) {
		failure.Category = report.SeriesFailureAnalysisCap
		failure.Reason = "Coralogix rejected /api/v1/series with ViolationTypeTotalSeriesAnalyzed — this metric has more time series in the lookback window than the server will analyze in one query. Reduce --series-lookback-hours or treat this metric as unanalyzable."
		return failure, true
	}

	var timeoutErr *coralogix.SeriesTimeoutError
	if errors.As(err, &timeoutErr) {
		failure.Category = report.SeriesFailureTimeout
		failure.Reason = fmt.Sprintf("the /api/v1/series query for this metric did not complete before the request timeout — usually a metric with a very large number of series (%s). Raise --timeout-sec, reduce --series-lookback-hours, or treat this metric as unanalyzable.", firstLine(timeoutErr.Cause))
		return failure, true
	}

	var statusErr *coralogix.HTTPStatusError
	if errors.As(err, &statusErr) {
		if isConfigLevelStatus(statusErr.Code) {
			return report.MetricSeriesFetchFailure{}, false
		}
		failure.Category = report.SeriesFailureServerError
		failure.Reason = fmt.Sprintf("Coralogix answered the /api/v1/series query for this metric with %s — commonly what the series endpoint does for a metric it cannot enumerate in the requested window. Reduce --series-lookback-hours to retry it, or treat this metric as unanalyzable.", statusErr.Status)
		return failure, true
	}

	failure.Category = report.SeriesFailureTransport
	failure.Reason = fmt.Sprintf("the /api/v1/series query for this metric failed: %s. The metric is excluded from the scan; everything else was scanned normally.", firstLine(err))
	return failure, true
}

// isConfigLevelStatus reports whether an HTTP status means the request was rejected on its own
// terms (credentials, permissions, URL, query syntax) rather than the server struggling with
// one metric. These are identical for every metric, so the scan aborts instead of skipping.
func isConfigLevelStatus(code int) bool {
	switch code {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed:
		return true
	}
	return false
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// firstLine reduces an error to its leading line, trimmed — client errors carry a multi-line
// curl replication block that would otherwise bloat every failure Reason in the report.
func firstLine(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimSpace(s), 200)
}

// omitCoralogixInternalMetricNames drops Coralogix-internal "__name__" values (prefix "cx_") before we
// fetch series or billing: customers never ship those metrics; they only add catalog noise.
func omitCoralogixInternalMetricNames(names []string) ([]string, int) {
	out := make([]string, 0, len(names))
	skipped := 0
	for _, n := range names {
		if promqlextract.IsCoralogixInternalMetricName(n) {
			skipped++
			continue
		}
		out = append(out, n)
	}
	return out, skipped
}

func toReportBilling(u metricusage.UnitsUsage) report.SeriesBilling {
	return report.SeriesBilling{
		UnitUsage:   u.UnitUsage,
		BytesVolume: u.BytesVolume,
		Cardinality: u.Cardinality,
		SampleCount: u.SampleCount,
		DaysInRange: u.DaysInRange,
	}
}

func billingPtr(m map[string]report.SeriesBilling, key string) *report.SeriesBilling {
	b, ok := m[key]
	if !ok {
		return nil
	}
	return &b
}
