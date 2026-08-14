package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func populatedReport() *Report {
	return &Report{
		Meta: Meta{
			APIHost:                      "api.eu2.coralogix.com",
			SeriesLookbackSeconds:        90000,
			SeriesLimitPerMetric:         50000,
			DashboardsScanned:            2,
			AlertsScanned:                3,
			SLOsScanned:                  1,
			DistinctMetricNames:          4,
			DistinctSeriesInCatalog:      3,
			SeriesFetchFailuresCount:     2,
			SeriesFetchTimeoutsCount:     1,
			DistinctLabelStringsRetained: 9,
		},
		Dashboards: []DashboardRef{{ID: "d1", Name: `Prod "overview"`}},
		UsedSeriesInCatalog: []UsedSeries{{
			Series: `m1{job="j"}`,
			Labels: map[string]string{"__name__": "m1", "job": "j"},
			Usage:  UsageCounts{Dashboards: 1, Total: 1},
			Billing: &SeriesBilling{
				UnitUsage: 12.5, BytesVolume: 100, Cardinality: 3, SampleCount: 42, DaysInRange: 7,
			},
		}},
		UnusedSeriesInCatalog: []UnusedSeries{
			{Series: `m2{job="j"}`, Labels: map[string]string{"__name__": "m2", "job": "j"}},
			{
				Series:  `m3{pod="p-1",tricky="quote\" and \\ backslash <html> & ampersand"}`,
				Labels:  map[string]string{"__name__": "m3", "pod": "p-1", "tricky": `quote" and \ backslash <html> & ampersand`},
				Billing: &SeriesBilling{UnitUsage: 0.25, DaysInRange: 7},
			},
		},
		ReferencedSelectorsWithoutMetricName: []SelectorRefIssue{{Kind: "dashboard", ResourceID: "d1", Selector: "{}"}},
		// Left nil on purpose: encoding/json renders these as null, and the streamer must match.
		ReferencedSelectorsMetricAbsentInTimeseriesWindow:  nil,
		ReferencedSelectorsMetricPresentButNoSeriesMatches: []SelectorRefIssue{},
		SeriesFetchFailures: []MetricSeriesFetchFailure{
			{MetricName: "huge", Category: SeriesFailureTimeout, Reason: "timed out"},
			{MetricName: "capped", Category: SeriesFailureAnalysisCap, Reason: "refused"},
		},
		Warnings:                  []string{"one warning", "another\twith\ttabs"},
		BillingSplitCountBySeries: map[string]int{`m2{job="j"}`: 3},
	}
}

// The streaming summary writer must be byte-identical to what MarshalIndent produced, including
// for fields that are nil vs empty. This also catches a field added to Report but not to
// writeSummaryJSON: the encoder emits it, the streamer doesn't, the bytes differ.
func TestSummaryJSONMatchesMarshalIndent(t *testing.T) {
	r := populatedReport()

	path := filepath.Join(t.TempDir(), "summary.json")
	if err := writeSummaryJSON(path, r); err != nil {
		t.Fatalf("writeSummaryJSON: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	if string(got) != string(want) {
		t.Fatalf("streamed summary differs from MarshalIndent.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Every Report field with a json tag must be emitted by writeSummaryJSON. The byte comparison
// above already implies this for the populated report, but this check states the requirement
// directly so the failure explains itself.
func TestSummaryJSONCoversEveryReportField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.json")
	if err := writeSummaryJSON(path, populatedReport()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var emitted map[string]json.RawMessage
	if err := json.Unmarshal(raw, &emitted); err != nil {
		t.Fatalf("streamed summary is not valid JSON: %v", err)
	}

	rt := reflect.TypeOf(Report{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name, _, _ := splitTag(tag)
		if name == "" || name == "-" {
			continue
		}
		if _, ok := emitted[name]; !ok {
			t.Errorf("field %s (json:%q) is missing from writeSummaryJSON", rt.Field(i).Name, name)
		}
	}
}

func splitTag(tag string) (name, opts string, ok bool) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", false
}

func TestWriteJSONArrayFileMatchesMarshalIndent(t *testing.T) {
	r := populatedReport()
	rows := unusedRowsFromSeries(r.UnusedSeriesInCatalog, r.BillingSplitCountBySeries)

	cases := []struct {
		name string
		v    any
		emit func(path string) error
	}{
		{"unused series", r.UnusedSeriesInCatalog, func(p string) error { return writeJSONArrayFile(p, r.UnusedSeriesInCatalog) }},
		{"cost rows", rows, func(p string) error { return writeJSONArrayFile(p, rows) }},
		{"by metric", AggregateUnusedByMetric(rows), func(p string) error {
			return writeJSONArrayFile(p, AggregateUnusedByMetric(rows))
		}},
		{"empty", []UnusedSeries{}, func(p string) error { return writeJSONArrayFile(p, []UnusedSeries{}) }},
		{"nil", []UnusedSeries(nil), func(p string) error { return writeJSONArrayFile(p, []UnusedSeries(nil)) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.json")
			if err := tc.emit(path); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.MarshalIndent(tc.v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')
			if string(got) != string(want) {
				t.Fatalf("streamed array differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}
