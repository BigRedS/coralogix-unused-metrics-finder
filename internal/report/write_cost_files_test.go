package report

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	byCostJSON = "metric_usage_unused_by_cost.json"
	byCostCSV  = "metric_usage_unused_by_cost.csv"
)

// filesOnDisk lists basenames actually written to dir.
func filesOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// With no billing data the per-series cost files carry no cost at all and their "by cost" order
// collapses to the series name, making them a multi-gigabyte duplicate of the unused-series
// file. They must not be written — and their absence must be reflected in Write's return value,
// which the CLI prints.
func TestWriteSkipsCostFilesWithoutBillingData(t *testing.T) {
	r := populatedReport()
	r.Meta.SeriesWithBillingData = 0
	r.Meta.UsageLookbackDays = 0
	r.Meta.UsageBillingCalendarMonths = 0

	dir := t.TempDir()
	written, err := r.Write(dir, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, name := range []string{byCostJSON, byCostCSV} {
		if slices.Contains(written, name) {
			t.Errorf("%s reported as written with no billing data", name)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists on disk with no billing data (stat err: %v)", name, err)
		}
	}

	// The outputs that don't depend on billing must still be produced.
	for _, name := range []string{
		"metric_usage_summary.json",
		"metric_usage_unused_series.json",
		"metric_usage_unused_by_metric.json",
		"metric_usage_unused_by_metric.csv",
		"metric_usage_all_by_metric.csv",
		"metric_usage_otel_processors.yaml",
		"metric_usage_report.pdf",
	} {
		if !slices.Contains(written, name) {
			t.Errorf("%s missing from Write's return value; got %v", name, written)
		}
	}
	if got, want := len(filesOnDisk(t, dir)), len(written); got != want {
		t.Errorf("%d files on disk but Write reported %d", got, want)
	}
}

func TestWriteIncludesCostFilesWithBillingData(t *testing.T) {
	r := populatedReport()
	r.Meta.SeriesWithBillingData = 2

	dir := t.TempDir()
	written, err := r.Write(dir, "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, name := range []string{byCostJSON, byCostCSV} {
		if !slices.Contains(written, name) {
			t.Errorf("%s missing from Write's return value; got %v", name, written)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}
}

// The PDF must say why a cost figure is missing rather than printing a zero that reads as a
// measurement of zero cost.
func TestCostCellDistinguishesMissingFromZero(t *testing.T) {
	cases := []struct {
		name         string
		seriesWith   int
		lookbackDays int
		want         string
	}{
		{"has data", 2, 7, "1.5 M"},
		{"requested but empty", 0, 7, "unavailable - billing lookup returned no data"},
		{"never requested", 0, 0, "not collected - run with --billing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{Meta: Meta{
				SeriesWithBillingData: tc.seriesWith,
				UsageLookbackDays:     tc.lookbackDays,
			}}
			if got := costCell(r, "1.5 M"); got != tc.want {
				t.Errorf("costCell = %q, want %q", got, tc.want)
			}
		})
	}
}
