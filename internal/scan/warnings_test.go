package scan

import (
	"strings"
	"testing"
)

// The case this exists for: a connection-level failure produces one identical error per metric.
// Thousands of copies of the same paragraph must collapse to one entry that names the cause once.
func TestAddForFoldsIdenticalErrors(t *testing.T) {
	w := newWarningSet()
	const grpcErr = `rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: credentials: cannot check peer: missing selected ALPN property"`

	metrics := []string{"FE_amount_cx_count", "b_metric", "a_metric", "c_metric", "d_metric", "e_metric", "f_metric"}
	for _, m := range metrics {
		w.AddFor("billing lookup failed", "metric", m, grpcErr)
	}

	got := w.List()
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1 folded entry:\n%s", len(got), strings.Join(got, "\n"))
	}
	entry := got[0]

	for _, want := range []string{
		"billing lookup failed for 7 metrics",
		"missing selected ALPN property",
		`"a_metric"`, // subjects sorted, so the first five alphabetically appear
		"(+2 more)",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("folded warning missing %q:\n%s", want, entry)
		}
	}
	if strings.Count(entry, "ALPN") != 1 {
		t.Errorf("root cause repeated within the folded warning:\n%s", entry)
	}
	if strings.Contains(entry, `"f_metric"`) {
		t.Errorf("folded warning listed more than %d subjects:\n%s", foldedSubjectsShown, entry)
	}
}

// A one-off failure must keep its full detail — that detail is the diagnostic, and there is
// nothing to fold it with.
func TestAddForKeepsFullDetailForSingleSubject(t *testing.T) {
	w := newWarningSet()
	detail := "404 Not Found\n\n--- HTTP replication ---\nGET https://api.example/dashboards/abc"
	w.AddFor("dashboard fetch failed", "dashboard", "abc", detail)

	got := w.List()
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1", len(got))
	}
	if !strings.Contains(got[0], "HTTP replication") {
		t.Errorf("single-subject warning lost its detail:\n%s", got[0])
	}
	if !strings.Contains(got[0], `dashboard "abc"`) {
		t.Errorf("single-subject warning does not name the subject:\n%s", got[0])
	}
}

// Per-item details often differ below the first line (each carries its own URL). Folding keys on
// the summary line so those still collapse.
func TestAddForFoldsOnFirstLineOnly(t *testing.T) {
	w := newWarningSet()
	w.AddFor("dashboard fetch failed", "dashboard", "d1", "500 Internal Server Error\n\nGET https://api/dashboards/d1")
	w.AddFor("dashboard fetch failed", "dashboard", "d2", "500 Internal Server Error\n\nGET https://api/dashboards/d2")

	got := w.List()
	if len(got) != 1 {
		t.Fatalf("got %d warnings, want 1 folded entry:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "for 2 dashboards") {
		t.Errorf("expected a folded count:\n%s", got[0])
	}
}

// Different problems must stay separate, whatever the subject.
func TestAddForKeepsDistinctErrorsApart(t *testing.T) {
	w := newWarningSet()
	w.AddFor("billing lookup failed", "metric", "a", "rpc error: Unavailable")
	w.AddFor("billing lookup failed", "metric", "b", "rpc error: PermissionDenied")
	w.AddFor("dashboard fetch failed", "dashboard", "d1", "rpc error: Unavailable")

	if got := len(w.List()); got != 3 {
		t.Fatalf("got %d warnings, want 3 distinct entries:\n%s", got, strings.Join(w.List(), "\n"))
	}
}

// Standalone warnings pass through untouched, in the order they were added, interleaved with
// folded groups by first appearance.
func TestListPreservesOrder(t *testing.T) {
	w := newWarningSet()
	w.Add("skipped alerts (--skip-alerts)")
	w.AddFor("billing lookup failed", "metric", "a", "boom")
	w.Add("%d of %d metrics skipped", 3, 10)
	w.AddFor("billing lookup failed", "metric", "b", "boom")

	got := w.List()
	if len(got) != 3 {
		t.Fatalf("got %d warnings, want 3:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got[0] != "skipped alerts (--skip-alerts)" {
		t.Errorf("first warning = %q", got[0])
	}
	if !strings.Contains(got[1], "for 2 metrics") {
		t.Errorf("second warning should be the folded group: %q", got[1])
	}
	if got[2] != "3 of 10 metrics skipped" {
		t.Errorf("third warning = %q", got[2])
	}
}

func TestWarningSetLenCountsAfterFolding(t *testing.T) {
	w := newWarningSet()
	for i := 0; i < 100; i++ {
		w.AddFor("billing lookup failed", "metric", string(rune('a'+i%26)), "same error")
	}
	if got := w.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
}
