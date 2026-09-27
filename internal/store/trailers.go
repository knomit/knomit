package store

import "strings"

// TrailerValue returns the value of the `Key: value` trailer line in a commit
// message, or "" when absent. go-git has no trailer parser and knomit's
// commit_log keeps only the first line, so this reads the message itself:
// trailers are the `Key: value` lines of the LAST paragraph, and the last
// occurrence of the key wins (git's own convention). The key is matched
// case-insensitively.
//
// F07 PR 1 only READS trailers (`Knomit-Trace` on the firing commit); nothing
// stamps one until PR 3. The value is returned trimmed, never parsed further.
func TrailerValue(message, key string) string {
	msg := strings.TrimRight(message, "\n")
	// The last paragraph: everything after the final blank line. A message
	// with a single paragraph is its own last paragraph, so a trailer written
	// straight under the subject line still reads.
	last := msg
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		last = msg[i+2:]
	}
	value, found := "", false
	for _, line := range strings.Split(last, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		value, found = strings.TrimSpace(v), true
	}
	if !found {
		return ""
	}
	return value
}
