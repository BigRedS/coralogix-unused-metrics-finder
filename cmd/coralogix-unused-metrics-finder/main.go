// Command coralogix-unused-metrics-finder scans Coralogix dashboards, alerts, and SLOs for PromQL
// metric references and compares them to the live metrics catalog.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/coralogix"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/cxteams"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/metricusage"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/region"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/report"
	"github.com/BigRedS/coralogix-unused-metrics-finder/internal/scan"
)

// resolveTeamFilenamePrefix attempts to discover the Coralogix team name attached to
// the API key and returns a filesystem-safe prefix to prepend to output files. Any
// failure (PermissionDenied, network, empty response) yields "" so the caller falls
// back to unprefixed filenames — team-name lookup is a nice-to-have, not required.
func resolveTeamFilenamePrefix(ctx context.Context, grpcHost, apiKey string) string {
	tc, err := cxteams.NewClient(grpcHost, apiKey)
	if err != nil {
		return ""
	}
	defer tc.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, err := tc.FetchTeamName(cctx)
	if err != nil || name == "" {
		return ""
	}
	return cxteams.SanitizeForFilename(name)
}

func runDebugBilling(ctx context.Context, billing *metricusage.Client, metric string, usageDays, usageMonths int) int {
	now := time.Now().UTC().Truncate(24 * time.Hour)
	var startDay, endDay time.Time
	if usageMonths > 0 {
		firstThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		endDay = firstThisMonth.AddDate(0, 0, -1)
		startDay = firstThisMonth.AddDate(0, -usageMonths, 0)
	} else {
		endDay = now
		startDay = endDay.AddDate(0, 0, -(usageDays - 1))
	}

	fmt.Fprintf(os.Stderr, "debug: metric=%q start=%s end=%s\n", metric, startDay.Format("2006-01-02"), endDay.Format("2006-01-02"))
	resp, err := billing.DebugRawVariations(ctx, metric, startDay, endDay)
	if err != nil {
		fmt.Fprintln(os.Stderr, "debug fetch failed:", err)
		return 1
	}

	days := resp.GetDailyUsages()
	fmt.Printf("daily_usages: %d day(s) returned\n", len(days))
	for di, d := range days {
		md := d.GetMetricDailyUsage()
		date := md.GetDate()
		stats := d.GetOutputSetStats()
		vars := d.GetVariationUsages()
		fmt.Printf("\n[day %d] date=%04d-%02d-%02d matched_count=%d daily_unit_usage=%g daily_bytes_volume=%d daily_cardinality=%d variation_usages_in_page=%d total_sample_count=%d\n",
			di, date.GetYear(), date.GetMonth(), date.GetDay(),
			stats.GetMatchedCount(),
			md.GetDailyUnitUsage(),
			md.GetDailyBytesVolume(),
			md.GetDailyCardinality(),
			len(vars),
			d.GetTotalSampleCount(),
		)
		const sampleN = 5
		for vi, v := range vars {
			if vi >= sampleN {
				fmt.Printf("  ... (%d more variations)\n", len(vars)-sampleN)
				break
			}
			u := v.GetUsage()
			fmt.Printf("  variation[%d] label_names=%v unit_usage=%g bytes_volume=%d sample_count=%d cardinality=%d\n",
				vi, v.GetLabelNames(),
				u.GetUnitUsage(), u.GetBytesVolume(), v.GetSampleCount(), u.GetCardinality(),
			)
		}
	}
	return 0
}

func main() {
	os.Exit(run())
}

func run() int {
	regionFlag := flag.String("region", "", "Coralogix region or domain (eu1, eu2.coralogix.com, api.eu2.coralogix.com, …)")
	keyFlag := flag.String("key", "", "Coralogix API key (Bearer)")
	outputDir := flag.String("output-dir", ".", "directory for report outputs (JSON, CSV per-series + per-metric rollup, OTEL YAML)")
	lookbackHours := flag.Float64("series-lookback-hours", 25, "time window for Prometheus series discovery")
	seriesLimit := flag.Int("series-limit-per-metric", 50_000, "max series rows per metric name")
	workers := flag.Int("workers", 8, "parallel series fetches")
	timeoutSec := flag.Int("timeout-sec", 120, "HTTP client timeout per request")
	grpcHostFlag := flag.String("grpc-host", "", "override the gRPC endpoint for billing and team lookup (default: ng-api-grpc.<domain> derived from --region)")
	billing := flag.Bool("billing", false, "fetch CX billing data (unit_usage/bytes_volume per series) and write the cost-ranked outputs; off by default because those per-series files are large and the lookup is slow")
	usageDays := flag.Int("usage-lookback-days", 7, "with --billing: rolling inclusive UTC calendar days ending today for CX unit_usage (ignored without --billing)")
	usageMonths := flag.Int("usage-billing-calendar-months", 0, "if >0, CX unit_usage window is the last N complete UTC calendar months (overrides rolling days when both set); 0 uses rolling days only")
	skipBilling := flag.Bool("skip-billing", false, "deprecated and no longer needed: billing is off unless --billing is given; when set it forces billing off")
	skipDashboards := flag.Bool("skip-dashboards", false, "skip dashboard catalog and definitions (omit Dashboard preset)")
	skipAlerts := flag.Bool("skip-alerts", false, "skip alert definitions v3 (omit Alerts preset)")
	skipSLO := flag.Bool("skip-slo", false, "skip SLO list (omit SLO preset)")
	quiet := flag.Bool("quiet", false, "disable progress status line on stderr")
	debugBillingMetric := flag.String("debug-billing-metric", "", "if set, dump the raw GetVariationUsagesByMetric response for this metric over --usage-lookback-days and exit (skips the full scan)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Required flags: --region and --key\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *usageDays < 0 || *usageMonths < 0 {
		fmt.Fprintln(os.Stderr, "usage lookback days and billing calendar months must be non-negative")
		return 2
	}

	billingEnabled := *billing && !*skipBilling
	if billingEnabled && *usageDays == 0 && *usageMonths == 0 {
		fmt.Fprintln(os.Stderr, "--billing needs a window: set --usage-lookback-days (default 7) or --usage-billing-calendar-months")
		return 2
	}

	if *regionFlag == "" || *keyFlag == "" {
		flag.Usage()
		return 2
	}

	apiHost, err := region.ResolveAPIHost(*regionFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	// Billing and team lookup are gRPC, which lives on a different host from the REST API —
	// region.GRPCHost explains why the REST host cannot be used.
	grpcHost := region.GRPCHost(apiHost)
	if *grpcHostFlag != "" {
		grpcHost = *grpcHostFlag
	}

	client := coralogix.NewClient(apiHost, *keyFlag, time.Duration(*timeoutSec)*time.Second)
	ctx := context.Background()

	var billingClient *metricusage.Client
	if billingEnabled {
		billingClient, err = metricusage.NewClient(grpcHost, *keyFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "billing client:", err)
			return 1
		}
		defer billingClient.Close()
	}

	if *debugBillingMetric != "" {
		if billingClient == nil {
			fmt.Fprintln(os.Stderr, "--debug-billing-metric requires --billing (with a window: --usage-lookback-days or --usage-billing-calendar-months)")
			return 2
		}
		return runDebugBilling(ctx, billingClient, *debugBillingMetric, *usageDays, *usageMonths)
	}

	// Without --billing the window flags keep their defaults but must not reach the scan, or it
	// would report a billing window it never queried.
	usageLookbackDays, usageBillingMonths := 0, 0
	if billingEnabled {
		usageLookbackDays, usageBillingMonths = *usageDays, *usageMonths
	}

	rep, err := scan.Run(ctx, client, scan.Options{
		SeriesLookback:             time.Duration(*lookbackHours * float64(time.Hour)),
		SeriesLimitPerMetric:       *seriesLimit,
		Workers:                    *workers,
		UsageLookbackDays:          usageLookbackDays,
		UsageBillingCalendarMonths: usageBillingMonths,
		Billing:                    billingClient,
		Quiet:                      *quiet,
		SkipDashboards:             *skipDashboards,
		SkipAlerts:                 *skipAlerts,
		SkipSLOs:                   *skipSLO,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan failed:", err)
		return 1
	}

	teamPrefix := resolveTeamFilenamePrefix(ctx, grpcHost, *keyFlag)
	if teamPrefix != "" {
		fmt.Fprintf(os.Stderr, "Using team-name prefix %q on output files.\n", teamPrefix)
	}

	written, err := rep.Write(*outputDir, teamPrefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write output:", err)
		return 1
	}
	for _, name := range written {
		fmt.Printf("Wrote %s/%s\n", *outputDir, name)
	}
	m := rep.Meta
	fmt.Printf(
		"Scanned dashboards=%d alerts=%d slos=%d; catalog series=%d; used=%d; unused=%d; "+
			"referenced metric absent in lookback=%d; no label match=%d; no metric name=%d; "+
			"series with billing=%d; unused with billing=%d; coralogix cx_* metric names skipped=%d\n",
		m.DashboardsScanned, m.AlertsScanned, m.SLOsScanned,
		m.DistinctSeriesInCatalog,
		len(rep.UsedSeriesInCatalog),
		len(rep.UnusedSeriesInCatalog),
		len(rep.ReferencedSelectorsMetricAbsentInTimeseriesWindow),
		len(rep.ReferencedSelectorsMetricPresentButNoSeriesMatches),
		len(rep.ReferencedSelectorsWithoutMetricName),
		m.SeriesWithBillingData,
		m.UnusedSeriesWithBilling,
		m.CoralogixInternalMetricNamesSkipped,
	)
	if m.MetricsTruncatedAtSeriesLimit > 0 {
		fmt.Fprintf(os.Stderr,
			"Warning: %d metric(s) hit the per-metric series limit; unused list may be incomplete.\n",
			m.MetricsTruncatedAtSeriesLimit,
		)
	}
	if m.SeriesFetchFailuresCount > 0 {
		fmt.Fprintf(os.Stderr,
			"Warning: %d of %d metric(s) skipped — their series catalog could not be retrieved (%s); see series_fetch_failures in metric_usage_summary.json. Their usage status is unknown (not 'unused') and they are excluded from the OTEL fragment.\n",
			m.SeriesFetchFailuresCount,
			m.DistinctMetricNames,
			report.SeriesFetchFailureBreakdown(rep.SeriesFetchFailures),
		)
		fmt.Fprintln(os.Stderr,
			"         A shorter --series-lookback-hours (or a larger --timeout-sec for the timed-out ones) may bring them back into the scan.")
	}
	if *skipDashboards || *skipAlerts || *skipSLO {
		var skipped []string
		if *skipDashboards {
			skipped = append(skipped, "dashboards")
		}
		if *skipAlerts {
			skipped = append(skipped, "alerts")
		}
		if *skipSLO {
			skipped = append(skipped, "SLOs")
		}
		fmt.Fprintf(os.Stderr,
			"Note: correlation skipped for %s — used/unused and OTEL output reflect PromQL only from scanned sources (see report warnings).\n",
			strings.Join(skipped, ", "))
	}
	return 0
}
