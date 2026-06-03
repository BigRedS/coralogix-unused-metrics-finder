package report

import (
	"encoding/json"
	"os"
	"testing"
)

// Run with: PDF_VISUAL_OUT=/tmp/visual.pdf go test ./internal/report -run TestVisualPDF -v
func TestVisualPDF(t *testing.T) {
	dst := os.Getenv("PDF_VISUAL_OUT")
	src := os.Getenv("PDF_VISUAL_SRC")
	if dst == "" || src == "" {
		t.Skip("set PDF_VISUAL_OUT and PDF_VISUAL_SRC to render against a real summary JSON")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rows := unusedRowsFromSeries(r.UnusedSeriesInCatalog, r.BillingSplitCountBySeries)
	byMetric := AggregateUnusedByMetric(rows)
	plan := BuildOTELProcessorPlan(r.UsedSeriesInCatalog, r.UnusedSeriesInCatalog)
	if err := WritePDFReport(dst, &r, byMetric, plan, "SampleTeam"); err != nil {
		t.Fatalf("WritePDFReport: %v", err)
	}
	t.Logf("wrote %s", dst)
}
