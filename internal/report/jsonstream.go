package report

// Streaming JSON output.
//
// json.MarshalIndent builds the compact encoding of the whole value AND an indented copy of it
// in memory before a single byte is written; json.Encoder.SetIndent does the same internally.
// For an account with millions of series that is gigabytes of peak heap on top of the catalog
// the scan is already holding, and it was enough to get the process killed under memory
// pressure. The writers here format one array element at a time instead, so peak memory is
// proportional to the largest single row rather than to the whole report.
//
// Output is byte-for-byte identical to json.MarshalIndent(v, "", "  ") — see jsonstream_test.go,
// which pins that for every file the report writes.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// jsonIndent is the per-level indent, matching the MarshalIndent calls this replaced.
const jsonIndent = "  "

// writeBufSize buffers a megabyte of output per file so multi-million-row exports aren't
// syscall-bound.
const writeBufSize = 1 << 20

// createJSONFile opens path and hands a buffered writer to emit, flushing and closing after.
func createJSONFile(path string, emit func(w *bufio.Writer) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	w := bufio.NewWriterSize(f, writeBufSize)
	if err := emit(w); err != nil {
		return err
	}
	// bufio errors are sticky, so a failed write earlier surfaces here.
	return w.Flush()
}

// appendValue writes v as it would appear nested at depth levels inside an indented document.
// Intended for small values (Meta, a single row); large slices belong in appendArray.
func appendValue(w *bufio.Writer, v any, depth int) error {
	compact, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact, strings.Repeat(jsonIndent, depth), jsonIndent); err != nil {
		return err
	}
	_, err = w.Write(indented.Bytes())
	return err
}

// appendArray writes items as a JSON array nested at depth, encoding one element at a time so
// the full array never exists as a single buffer. A nil slice renders as "null" and an empty
// non-nil slice as "[]", exactly as encoding/json does — several report fields are left nil when
// empty, and consumers of the existing output see that distinction.
func appendArray[T any](w *bufio.Writer, items []T, depth int) error {
	if items == nil {
		_, err := w.WriteString("null")
		return err
	}
	if len(items) == 0 {
		_, err := w.WriteString("[]")
		return err
	}
	elemDepth := strings.Repeat(jsonIndent, depth+1)
	if _, err := w.WriteString("[\n"); err != nil {
		return err
	}
	// Both buffers are reused for every element: they grow to the largest single row and stay
	// there. Allocating per row instead produces garbage proportional to the whole document,
	// which is what pushes the heap high enough to matter on a multi-million-series account.
	var compact, indented bytes.Buffer
	enc := json.NewEncoder(&compact)
	for i, item := range items {
		compact.Reset()
		if err := enc.Encode(item); err != nil {
			return fmt.Errorf("encode element %d: %w", i, err)
		}
		// Encode appends a newline that json.Indent would treat as content.
		row := bytes.TrimRight(compact.Bytes(), "\n")

		indented.Reset()
		if err := json.Indent(&indented, row, elemDepth, jsonIndent); err != nil {
			return err
		}
		w.WriteString(elemDepth)
		w.Write(indented.Bytes())
		if i < len(items)-1 {
			w.WriteString(",")
		}
		w.WriteString("\n")
	}
	_, err := w.WriteString(strings.Repeat(jsonIndent, depth) + "]")
	return err
}

// writeJSONArrayFile writes items to path as a top-level indented JSON array.
func writeJSONArrayFile[T any](path string, items []T) error {
	return createJSONFile(path, func(w *bufio.Writer) error {
		if err := appendArray(w, items, 0); err != nil {
			return err
		}
		_, err := w.WriteString("\n")
		return err
	})
}

// objectWriter emits an indented JSON object field by field. Fields must be written in struct
// declaration order to match encoding/json's output.
type objectWriter struct {
	w     *bufio.Writer
	depth int // depth of the object itself; fields sit one level deeper
	n     int
	err   error
}

func (o *objectWriter) open() {
	if o.err == nil {
		_, o.err = o.w.WriteString("{")
	}
}

// key writes the separator, newline and quoted field name, leaving the writer positioned for
// the value.
func (o *objectWriter) key(name string) bool {
	if o.err != nil {
		return false
	}
	if o.n > 0 {
		o.w.WriteString(",")
	}
	o.n++
	o.w.WriteString("\n" + strings.Repeat(jsonIndent, o.depth+1))
	compactName, err := json.Marshal(name)
	if err != nil {
		o.err = err
		return false
	}
	o.w.Write(compactName)
	_, o.err = o.w.WriteString(": ")
	return o.err == nil
}

// value writes a small field value (anything not a large slice).
func (o *objectWriter) value(name string, v any) {
	if !o.key(name) {
		return
	}
	o.err = appendValue(o.w, v, o.depth+1)
}

func (o *objectWriter) close() error {
	if o.err != nil {
		return o.err
	}
	if o.n == 0 {
		_, err := o.w.WriteString("}")
		return err
	}
	_, err := o.w.WriteString("\n" + strings.Repeat(jsonIndent, o.depth) + "}")
	return err
}

// arrayField writes a slice field, streaming its elements. A free function because Go methods
// cannot be generic.
func arrayField[T any](o *objectWriter, name string, items []T) {
	if !o.key(name) {
		return
	}
	o.err = appendArray(o.w, items, o.depth+1)
}

// writeSummaryJSON streams the full report to path.
//
// Field order and names must match the Report struct's json tags — TestSummaryJSONMatchesMarshalIndent
// fails if a field is added to Report without being added here, so the two cannot drift silently.
func writeSummaryJSON(path string, r *Report) error {
	return createJSONFile(path, func(w *bufio.Writer) error {
		o := &objectWriter{w: w}
		o.open()
		o.value("meta", r.Meta)
		arrayField(o, "dashboards", r.Dashboards)
		arrayField(o, "used_series_in_catalog", r.UsedSeriesInCatalog)
		arrayField(o, "unused_series_in_catalog", r.UnusedSeriesInCatalog)
		arrayField(o, "referenced_selectors_without_metric_name", r.ReferencedSelectorsWithoutMetricName)
		arrayField(o, "referenced_selectors_metric_absent_in_timeseries_window", r.ReferencedSelectorsMetricAbsentInTimeseriesWindow)
		arrayField(o, "referenced_selectors_metric_present_but_no_series_matches", r.ReferencedSelectorsMetricPresentButNoSeriesMatches)
		// omitempty on the struct field: an empty list is left out entirely.
		if len(r.SeriesFetchFailures) > 0 {
			arrayField(o, "series_fetch_failures", r.SeriesFetchFailures)
		}
		arrayField(o, "warnings", r.Warnings)
		if err := o.close(); err != nil {
			return err
		}
		_, err := w.WriteString("\n")
		return err
	})
}
