package fact

import (
	"fmt"
	"strings"
)

// CheckFenceEdit refuses an edit that UNBALANCES the body's triple-backtick
// fences: before has an even number of fence lines and after has an odd one.
// An unclosed fence turns everything after it into code, so one stray line
// silently breaks the rest of the fact's rendering.
//
// It judges the edit, not the result. Valid CommonMark can hold an odd count —
// a ```` fence quoting a ``` line, ``` inside a ~~~ fence, indented code, an
// inline ```x``` at the start of a line — and a fact like that must stay
// editable, so a body that was already odd is never refused here.
//
// A fence line is any line whose first non-blank characters are "```". The
// rule is a count, not a CommonMark parse, so its answer is deterministic.
// knomit_update is the only write path that runs it.
func CheckFenceEdit(before, after string) error {
	was, _ := fenceLines(before)
	now, last := fenceLines(after)
	if was%2 == 0 && now%2 != 0 {
		return fmt.Errorf("the edit unbalances the body's code fences: %d ``` fence lines, the last at line %d — close the fence or remove the stray one",
			now, last)
	}
	return nil
}

// fenceLines counts the fence lines in body and returns the 1-based number of
// the last one.
func fenceLines(body string) (count, last int) {
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			count++
			last = i + 1
		}
	}
	return count, last
}
