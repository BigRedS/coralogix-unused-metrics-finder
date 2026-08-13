package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func benchRows(n int) []UnusedByCostRow {
	rows := make([]UnusedByCostRow, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%d", i)
		rows = append(rows, UnusedByCostRow{
			Series:     `process_memory_usage_By{pod="checkout-7d9f8b6c4d-` + id + `"}`,
			MetricName: "process_memory_usage_By",
			Labels: map[string]string{
				"__name__":     "process_memory_usage_By",
				"job":          "kubernetes-pods",
				"namespace":    "production",
				"node":         "ip-10-0-1-12.eu-west-1.compute.internal",
				"pod":          "checkout-7d9f8b6c4d-" + id,
				"service_name": "checkout-service",
			},
			BillingPresent: true,
			UnitUsage:      1.25,
			BytesVolume:    4096,
			SampleCount:    2016,
			Cardinality:    1,
			DaysInRange:    7,
		})
	}
	return rows
}

// BenchmarkWriteArray_MarshalIndent is the approach this package used to take: build the whole
// document (twice — compact, then indented) in memory, then write it.
func BenchmarkWriteArray_MarshalIndent(b *testing.B) {
	rows := benchRows(100_000)
	dir := b.TempDir()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		blob, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "out.json"), blob, 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteArray_Streamed formats one row at a time into a buffered writer.
func BenchmarkWriteArray_Streamed(b *testing.B) {
	rows := benchRows(100_000)
	dir := b.TempDir()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := writeJSONArrayFile(filepath.Join(dir, "out.json"), rows); err != nil {
			b.Fatal(err)
		}
	}
}
