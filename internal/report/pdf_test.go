package report

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWritePDFReport(t *testing.T) {
	t.Parallel()

	r := &Report{
		Meta: Meta{
			APIHost:                  "api.eu2.coralogix.com",
			SeriesLookbackSeconds:    25 * 3600,
			SeriesLimitPerMetric:     50_000,
			DashboardsScanned:        12,
			AlertsScanned:            34,
			SLOsScanned:              5,
			DistinctMetricNames:      20,
			DistinctSeriesInCatalog:  100,
			UsageLookbackDays:        7,
			UsageBillingUTCStartDate: "2026-05-21",
			UsageBillingUTCEndDate:   "2026-05-27",
			SeriesWithBillingData:    80,
			UnusedSeriesWithBilling:  30,
		},
		UsedSeriesInCatalog: []UsedSeries{
			{Series: `up{job="api"}`, Labels: map[string]string{"__name__": "up", "job": "api"}},
		},
		UnusedSeriesInCatalog: []UnusedSeries{
			{
				Series:  `requests_total{job="legacy",zone="z1"}`,
				Labels:  map[string]string{"__name__": "requests_total", "job": "legacy", "zone": "z1"},
				Billing: &SeriesBilling{UnitUsage: 12.5, BytesVolume: 1_048_576, SampleCount: 1000, Cardinality: 4, DaysInRange: 7},
			},
			{
				Series:  `up{job="legacy"}`,
				Labels:  map[string]string{"__name__": "up", "job": "legacy"},
				Billing: &SeriesBilling{UnitUsage: 3.0, BytesVolume: 100_000, SampleCount: 200, Cardinality: 2, DaysInRange: 7},
			},
			{
				Series: `pet_count{shop="bag"}`,
				Labels: map[string]string{"__name__": "pet_count", "shop": "bag"},
			},
		},
		Warnings: []string{"--skip-slo set: SLO references not considered"},
	}

	rows := unusedRowsFromSeries(r.UnusedSeriesInCatalog, nil)
	byMetric := AggregateUnusedByMetric(rows)
	plan := BuildOTELProcessorPlan(r.UsedSeriesInCatalog, r.UnusedSeriesInCatalog)

	dir := t.TempDir()
	path := filepath.Join(dir, "out.pdf")
	if err := WritePDFReport(path, r, byMetric, plan, "TestTeam"); err != nil {
		t.Fatalf("WritePDFReport: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if len(got) < 1024 {
		t.Fatalf("pdf suspiciously small: %d bytes", len(got))
	}
	if !bytes.HasPrefix(got, []byte("%PDF-")) {
		t.Fatalf("missing %%PDF- header, got %q", got[:min(8, len(got))])
	}
}
