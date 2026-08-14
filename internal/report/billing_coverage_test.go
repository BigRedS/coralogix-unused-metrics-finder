package report

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBillingCoverageAndSufficiency(t *testing.T) {
	cases := []struct {
		name           string
		attempted      int
		succeeded      int
		partialOK      bool
		wantCoverage   float64
		wantSufficient bool
	}{
		{"full coverage", 100, 100, false, 1.0, true},
		{"at threshold", 100, 90, false, 0.90, true},
		{"just below threshold", 100, 89, false, 0.89, false},
		{"badly incomplete", 1000, 12, false, 0.012, false},
		{"badly incomplete but accepted", 1000, 12, true, 0.012, true},
		{"nothing attempted makes no claim", 0, 0, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{Meta: Meta{
				BillingMetricLookupsAttempted: tc.attempted,
				BillingMetricLookupsSucceeded: tc.succeeded,
				BillingPartialAccepted:        tc.partialOK,
			}}
			if got := r.BillingCoverage(); got < tc.wantCoverage-0.0001 || got > tc.wantCoverage+0.0001 {
				t.Errorf("BillingCoverage = %v, want %v", got, tc.wantCoverage)
			}
			if got := r.BillingCoverageSufficient(); got != tc.wantSufficient {
				t.Errorf("BillingCoverageSufficient = %v, want %v", got, tc.wantSufficient)
			}
		})
	}
}

// Partial coverage mis-ranks cost — an unmeasured metric looks free — so the cost-ranked files
// must be withheld even though billing data exists, unless the caller accepted partial data.
func TestWriteWithholdsCostFilesBelowCoverageThreshold(t *testing.T) {
	cases := []struct {
		name      string
		succeeded int
		partialOK bool
		wantFiles bool
	}{
		{"sufficient coverage", 95, false, true},
		{"insufficient coverage", 40, false, false},
		{"insufficient but accepted", 40, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := populatedReport()
			r.Meta.SeriesWithBillingData = 2
			r.Meta.BillingMetricLookupsAttempted = 100
			r.Meta.BillingMetricLookupsSucceeded = tc.succeeded
			r.Meta.BillingPartialAccepted = tc.partialOK

			dir := t.TempDir()
			written, err := r.Write(dir, "")
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			for _, name := range []string{byCostJSON, byCostCSV} {
				got := slices.Contains(written, name)
				if got != tc.wantFiles {
					t.Errorf("%s written = %v, want %v", name, got, tc.wantFiles)
				}
				_, statErr := os.Stat(filepath.Join(dir, name))
				if exists := statErr == nil; exists != tc.wantFiles {
					t.Errorf("%s on disk = %v, want %v", name, exists, tc.wantFiles)
				}
			}
		})
	}
}

func TestCostCellExplainsWithheldCoverage(t *testing.T) {
	r := populatedReport()
	r.Meta.SeriesWithBillingData = 2
	r.Meta.BillingMetricLookupsAttempted = 100
	r.Meta.BillingMetricLookupsSucceeded = 40

	got := costCell(r, "1.5 M")
	want := "withheld - billing covered only 40% of metrics (need 90%)"
	if got != want {
		t.Errorf("costCell = %q, want %q", got, want)
	}

	// Accepted partial data shows the figure — the warning and the coverage row carry the caveat.
	r.Meta.BillingPartialAccepted = true
	if got := costCell(r, "1.5 M"); got != "1.5 M" {
		t.Errorf("costCell with accepted partial data = %q, want the figure", got)
	}
}
