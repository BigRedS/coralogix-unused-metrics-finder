package scan

import (
	"fmt"
	"sort"
	"strings"
)

// Warning folding.
//
// Several scan steps iterate over many items — one call per dashboard, one billing lookup per
// metric name — and a failure that is really one problem (a dead connection, an expired key)
// then produces one warning per item. A report with the same paragraph repeated two thousand
// times is unreadable, and it disguises a single root cause as a per-item pattern.
//
// warningSet folds warnings that share a summary line into one entry naming the affected
// subjects, and leaves genuinely one-off warnings exactly as they were.

// foldedSubjectsShown caps how many affected subjects a folded warning names before switching to
// a count. Enough to recognise the pattern; short enough to stay one readable line.
const foldedSubjectsShown = 5

// warningSet accumulates scan warnings in order, folding repeats of the same problem.
type warningSet struct {
	entries []*warningEntry
	byKey   map[string]*warningEntry
}

type warningEntry struct {
	// standalone is a warning that is never folded (set by Add).
	standalone string

	// The rest describe a foldable group (set by AddFor).
	context  string // what was being attempted, e.g. "billing lookup failed"
	noun     string // subject kind, e.g. "metric" — pluralised with "s" when folded
	detail   string // full detail of the first occurrence, kept for the single-subject case
	subjects []string
}

func newWarningSet() *warningSet {
	return &warningSet{byKey: make(map[string]*warningEntry)}
}

// Add records a warning as-is. Use for one-off conditions ("skipped dashboards"), not for
// per-item failures.
func (w *warningSet) Add(format string, args ...any) {
	if w == nil {
		return
	}
	w.entries = append(w.entries, &warningEntry{standalone: fmt.Sprintf(format, args...)})
}

// AddFor records a failure of one subject (a metric name, a dashboard ID) whose detail is likely
// shared with others. Warnings with the same context and the same first line of detail fold into
// a single entry. context describes the attempt ("billing lookup failed"); noun names what the
// subject is ("metric").
func (w *warningSet) AddFor(context, noun, subject, detail string) {
	if w == nil {
		return
	}
	// Fold on the first line: per-item details often carry the subject's own URL or id further
	// down (an HTTP replication block, for instance), which would defeat exact-match grouping
	// while the summary line is identical.
	key := context + "\x00" + noun + "\x00" + firstLineOf(detail)
	if e, ok := w.byKey[key]; ok {
		e.subjects = append(e.subjects, subject)
		return
	}
	e := &warningEntry{
		context:  context,
		noun:     noun,
		detail:   detail,
		subjects: []string{subject},
	}
	w.byKey[key] = e
	w.entries = append(w.entries, e)
}

// Len reports how many warnings will be emitted, after folding.
func (w *warningSet) Len() int {
	if w == nil {
		return 0
	}
	return len(w.entries)
}

// List renders the warnings in the order their first occurrence was recorded.
func (w *warningSet) List() []string {
	if w == nil || len(w.entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.entries))
	for _, e := range w.entries {
		out = append(out, e.render())
	}
	return out
}

func (e *warningEntry) render() string {
	if e.standalone != "" {
		return e.standalone
	}

	// One subject: no folding to do, so keep the full detail — it's the diagnostic.
	if len(e.subjects) == 1 {
		return fmt.Sprintf("%s for %s %q: %s", e.context, e.noun, e.subjects[0], e.detail)
	}

	// Subjects arrive from parallel workers; sort for a stable report.
	subjects := make([]string, len(e.subjects))
	copy(subjects, e.subjects)
	sort.Strings(subjects)

	shown := subjects
	if len(shown) > foldedSubjectsShown {
		shown = shown[:foldedSubjectsShown]
	}
	quoted := make([]string, 0, len(shown))
	for _, s := range shown {
		quoted = append(quoted, fmt.Sprintf("%q", s))
	}
	affected := strings.Join(quoted, ", ")
	if remaining := len(subjects) - len(shown); remaining > 0 {
		affected += fmt.Sprintf(" (+%d more)", remaining)
	}

	return fmt.Sprintf("%s for %d %ss, all with the same error: %s — affected: %s",
		e.context, len(subjects), e.noun, firstLineOf(e.detail), affected)
}

// firstLineOf returns the leading line of s, trimmed and length-capped. Client errors carry a
// multi-line curl replication block that would bury the summary in a folded warning.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimSpace(s), 300)
}
