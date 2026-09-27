package fact

import (
	"fmt"
	"strings"
)

// CheckFences refuses a body with an odd number of triple-backtick fence
// lines. An unclosed fence turns everything after it into code, so one stray
// line silently breaks the rest of the fact's rendering.
//
// A fence line is any line whose first non-blank characters are "```". The
// rule is a count, not a CommonMark parse: deterministic, and the same answer
// on every write path.
func CheckFences(body string) error {
	count, last := 0, 0
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			count++
			last = i + 1
		}
	}
	if count%2 != 0 {
		return fmt.Errorf("body has an unbalanced code fence: %d ``` fence lines, the last at line %d — close the fence or remove the stray one",
			count, last)
	}
	return nil
}
