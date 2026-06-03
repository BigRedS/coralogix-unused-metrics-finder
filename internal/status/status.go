// Package status provides a single-line progress display on stderr.
package status

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Line writes one status message, overwriting the previous line when stderr is a TTY.
type Line struct {
	w           io.Writer
	mu          sync.Mutex
	interactive bool
	width       int
	lastPct     int // last whole-percent emitted by Bar (non-TTY throttle); -1 = none
}

// New creates a status line writing to w (typically os.Stderr).
func New(w io.Writer) *Line {
	interactive := false
	width := 100
	if f, ok := w.(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
		if interactive {
			if tw, _, err := term.GetSize(int(f.Fd())); err == nil && tw > 20 {
				width = tw
			}
		}
	}
	return &Line{w: w, interactive: interactive, width: width, lastPct: -1}
}

// Set updates the current status text.
func (l *Line) Set(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.interactive {
		fmt.Fprintln(l.w, msg)
		return
	}
	if len(msg) > l.width-1 {
		msg = msg[:l.width-4] + "..."
	}
	pad := l.width - len(msg)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(l.w, "\r%s%s", msg, strings.Repeat(" ", pad))
}

// Bar renders an in-place progress bar of the form
//
//	label [######......]  47%  3204/6874  suffix
//
// On a TTY it overwrites the current line; on a non-TTY it prints one line only
// when the whole-number percentage changes (and at completion), to avoid log
// spam from thousands of updates. total <= 0 renders an empty (0%) bar.
func (l *Line) Bar(label string, done, total int, suffix string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if total < 0 {
		total = 0
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	pct := 0
	if total > 0 {
		pct = int(float64(done) / float64(total) * 100)
	}

	if !l.interactive {
		if pct == l.lastPct && done != total {
			return
		}
		l.lastPct = pct
		msg := fmt.Sprintf("%s %d%% (%d/%d)", label, pct, done, total)
		if suffix != "" {
			msg += " " + suffix
		}
		fmt.Fprintln(l.w, msg)
		return
	}

	// Layout: "<label> [<bar>] <stats>  <suffix>", bar absorbs the slack.
	stats := fmt.Sprintf(" %3d%% %d/%d", pct, done, total)
	suffixPart := ""
	if suffix != "" {
		suffixPart = "  " + suffix
	}
	// Fixed columns consumed by everything except the bar fill: label, " [", "]",
	// stats, suffix, and a 1-col right margin.
	fixed := len(label) + 2 + 1 + len(stats) + len(suffixPart) + 1
	barWidth := l.width - fixed

	// If the suffix crowds out the bar, trim or drop it to keep a usable bar.
	const minBar = 10
	if barWidth < minBar && suffixPart != "" {
		room := l.width - (len(label) + 2 + 1 + len(stats) + 1) - minBar - 2
		if room > 3 {
			suffixPart = "  " + suffix[:min(len(suffix), room)]
		} else {
			suffixPart = ""
		}
		fixed = len(label) + 2 + 1 + len(stats) + len(suffixPart) + 1
		barWidth = l.width - fixed
	}

	if barWidth < 1 {
		// Terminal too narrow for a bar; fall back to text like Set.
		msg := label + stats
		if len(msg) > l.width-1 {
			msg = msg[:l.width-1]
		}
		l.overwrite(msg)
		return
	}

	filled := 0
	if total > 0 {
		filled = int(float64(barWidth) * float64(done) / float64(total))
	}
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("#", filled) + strings.Repeat(".", barWidth-filled)
	l.overwrite(fmt.Sprintf("%s [%s]%s%s", label, bar, stats, suffixPart))
}

// overwrite redraws line content in place, padding to clear any prior text.
// Caller must hold l.mu and only call this on an interactive terminal.
func (l *Line) overwrite(s string) {
	if len(s) > l.width-1 {
		s = s[:l.width-1]
	}
	pad := l.width - len(s)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(l.w, "\r%s%s", s, strings.Repeat(" ", pad))
}

// Done clears the in-progress line and optionally prints a final message.
func (l *Line) Done(final string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.interactive {
		fmt.Fprintf(l.w, "\r%s\r\n", strings.Repeat(" ", l.width))
	}
	if final != "" {
		fmt.Fprintln(l.w, final)
	}
}
