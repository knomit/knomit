package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ui renders staged progress for a long-running command.
//
// Every stage is announced BEFORE the work starts, not after it finishes:
// fetching a large knowledge base and walking its history each take seconds to
// minutes, and a silent process is indistinguishable from a hung one.
//
// On a terminal a stage's line is rewritten in place as counters advance and
// again when it completes. Everywhere else — a pipe, a CI log, a file — each
// stage prints exactly one line when it finishes, so the output stays readable
// without carriage returns or escape codes.
type ui struct {
	w        io.Writer
	tty      bool
	color    bool
	start    time.Time
	label    string
	detail   string
	stepAt   time.Time
	open     bool // a stage is in progress
	lastDraw int  // width of the last in-place line, to erase it cleanly
}

const labelWidth = 11

func newUI(w io.Writer) *ui {
	tty := isTerminal(w)
	return &ui{
		w:     w,
		tty:   tty,
		color: tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
		start: time.Now(),
	}
}

// isTerminal reports whether w is a character device, i.e. an interactive
// terminal rather than a pipe or a file.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (u *ui) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *ui) dim(s string) string  { return u.paint("2", s) }
func (u *ui) bold(s string) string { return u.paint("1", s) }
func (u *ui) green(s string) string {
	return u.paint("32", s)
}

// Banner prints the tool identity once, at the top of a run.
func (u *ui) Banner(version string) {
	fmt.Fprintf(u.w, "\n%s %s\n\n", u.bold("knomit-okf"), u.dim(version))
}

// Step announces a stage and leaves it open. detail is what the stage is
// working ON (a URL, a branch) — the summary of what it PRODUCED comes later,
// from Done.
func (u *ui) Step(label, detail string) {
	u.closeOpen()
	u.label, u.detail, u.stepAt, u.open = label, detail, time.Now(), true
	if u.tty {
		u.draw("·", detail, "")
	}
}

// Update refreshes an open stage's detail in place. It is a no-op off a
// terminal, where rewriting a line is not possible and per-tick lines would
// bury the log.
func (u *ui) Update(detail string) {
	if !u.tty || !u.open {
		return
	}
	u.draw("·", detail, "")
}

// Done closes the open stage, replacing its detail with what it produced and
// the time it took.
func (u *ui) Done(summary string) {
	if !u.open {
		return
	}
	u.open = false
	u.draw(u.green("✓"), summary, humanDuration(time.Since(u.stepAt)))
	fmt.Fprintln(u.w)
	u.lastDraw = 0
}

// Skip closes the open stage as "nothing to do" — distinct from Done so a
// no-op sync reads as a no-op rather than as work performed.
func (u *ui) Skip(summary string) {
	if !u.open {
		return
	}
	u.open = false
	u.draw(u.dim("·"), u.dim(summary), "")
	fmt.Fprintln(u.w)
	u.lastDraw = 0
}

// draw renders one stage line, erasing the previous in-place line first.
func (u *ui) draw(symbol, detail, elapsed string) {
	label := u.label
	if len(label) < labelWidth {
		label += strings.Repeat(" ", labelWidth-len(label))
	}
	line := fmt.Sprintf("  %s %s %s", symbol, u.bold(label), detail)
	if elapsed != "" {
		line += "  " + u.dim(elapsed)
	}
	if !u.tty {
		fmt.Fprint(u.w, line)
		return
	}
	// Erase the previous line's tail so a shorter line cannot leave debris.
	pad := ""
	if n := u.lastDraw - visibleLen(line); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	u.lastDraw = visibleLen(line)
	fmt.Fprint(u.w, "\r"+line+pad)
}

// closeOpen terminates an open stage's line when a new stage starts without
// its predecessor having been closed (an error path).
func (u *ui) closeOpen() {
	if u.open {
		u.open = false
		fmt.Fprintln(u.w)
		u.lastDraw = 0
	}
}

// Note prints an aside that is not a stage — a warning from the loader, say.
func (u *ui) Note(format string, args ...any) {
	u.closeOpen()
	fmt.Fprintf(u.w, "    %s %s\n", u.dim("!"), fmt.Sprintf(format, args...))
}

// Finish prints the closing summary line.
func (u *ui) Finish(format string, args ...any) {
	u.closeOpen()
	fmt.Fprintf(u.w, "\n%s %s %s\n", u.green("✓"), fmt.Sprintf(format, args...),
		u.dim("in "+humanDuration(time.Since(u.start))))
}

// Hint prints follow-up commands, indented, after a successful run.
func (u *ui) Hint(title string, lines ...string) {
	fmt.Fprintf(u.w, "\n  %s\n", u.dim(title))
	for _, l := range lines {
		fmt.Fprintf(u.w, "    %s\n", l)
	}
	fmt.Fprintln(u.w)
}

// visibleLen is the printable width of s, ignoring ANSI escape sequences and
// counting runes rather than bytes — the label and summaries carry non-ASCII.
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

// humanDuration formats an elapsed time at a resolution a human cares about.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// plural returns "s" when n is not 1, so counts read as English.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// pluralES is plural for nouns taking "-es" ("branch" → "branches").
func pluralES(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}
