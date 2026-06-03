package status

import (
	"bytes"
	"strings"
	"testing"
)

// ttyLine builds a Line that renders as if attached to a terminal of the given
// width, writing to buf. (New() only enables interactive mode for *os.File.)
func ttyLine(buf *bytes.Buffer, width int) *Line {
	return &Line{w: buf, interactive: true, width: width, lastPct: -1}
}

func TestBarInteractiveRendering(t *testing.T) {
	var buf bytes.Buffer
	l := ttyLine(&buf, 80)
	l.Bar("metrics", 25, 100, "some_metric")

	out := buf.String()
	if !strings.HasPrefix(out, "\r") {
		t.Fatalf("expected carriage-return redraw, got %q", out)
	}
	for _, want := range []string{"metrics [", "]", " 25% 25/100", "some_metric"} {
		if !strings.Contains(out, want) {
			t.Fatalf("bar output %q missing %q", out, want)
		}
	}
	// Bar fill should be present and proportional (25% of the fill region).
	if !strings.Contains(out, "#") || !strings.Contains(out, ".") {
		t.Fatalf("expected a partially-filled bar, got %q", out)
	}
}

func TestBarFullAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	l := ttyLine(&buf, 80)

	l.Bar("metrics", 0, 100, "")
	if strings.Contains(buf.String(), "#") {
		t.Fatalf("0%% bar should have no fill: %q", buf.String())
	}

	buf.Reset()
	l.Bar("metrics", 100, 100, "")
	out := buf.String()
	if strings.Contains(out, ".") {
		t.Fatalf("100%% bar should be fully filled: %q", out)
	}
	if !strings.Contains(out, "100% 100/100") {
		t.Fatalf("expected 100%% stats, got %q", out)
	}
}

func TestBarNonInteractiveThrottle(t *testing.T) {
	var buf bytes.Buffer
	// New() on a bytes.Buffer yields interactive=false.
	l := New(&buf)

	// 200 updates across 0..100% should print once per whole percent (101 lines)
	// plus the throttle lets the final 100% through.
	for i := 0; i <= 200; i++ {
		l.Bar("metrics", i, 200, "m")
	}
	lines := strings.Count(buf.String(), "\n")
	if lines < 95 || lines > 105 {
		t.Fatalf("expected ~101 throttled lines, got %d:\n%s", lines, buf.String())
	}
	if !strings.Contains(buf.String(), "metrics 100% (200/200) m") {
		t.Fatalf("expected a final 100%% line, got:\n%s", buf.String())
	}
}

func TestBarNarrowTerminalFallback(t *testing.T) {
	var buf bytes.Buffer
	l := ttyLine(&buf, 24) // too narrow for label + long suffix + a real bar
	l.Bar("billing", 3, 10, "a_very_long_metric_name_that_will_not_fit")
	out := buf.String()
	if !strings.Contains(out, "billing") || !strings.Contains(out, "30%") {
		t.Fatalf("narrow fallback should still show label and percent, got %q", out)
	}
	// Must never exceed the terminal width (minus the carriage return).
	body := strings.TrimPrefix(out, "\r")
	if len(body) > l.width {
		t.Fatalf("line %d cols exceeds width %d: %q", len(body), l.width, body)
	}
}
