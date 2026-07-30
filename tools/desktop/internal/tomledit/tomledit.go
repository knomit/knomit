// Package tomledit makes targeted edits to a TOML file's text, leaving
// everything it was not asked to change exactly as it found it.
//
// The settings dialog writes knomit.toml, and knomit.toml is a file people
// hand-edit. Marshalling a Config struct back over it would be far less code,
// but it would delete every comment and silently drop any key the struct does
// not model — including keys a NEWER knomit understands and this build does
// not. So the file is treated as text.
//
// This is not a TOML parser. It handles the subset the settings dialog writes:
// top-level keys and simple `[table]` headers, with string values. Arrays of
// tables, dotted keys, and inline tables are out of scope and will simply not
// be matched, causing the key to be appended rather than updated.
package tomledit

import (
	"fmt"
	"strings"
)

// SetString returns src with table.key set to value, quoted as a TOML basic
// string. An empty table means the root table.
func SetString(src []byte, table, key, value string) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("tomledit: key must not be empty")
	}
	assignment := fmt.Sprintf("%s = %s", key, quote(value))

	lines := splitLines(string(src))
	inTarget := table == "" // the root table is open from line one
	sawTarget := inTarget
	insertAt := -1 // where to add the key if the table exists but the key does not

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if isTableHeader(trimmed) {
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if inTarget && insertAt < 0 {
				// Leaving the target table without having found the key: this
				// header is the first line that is NOT ours, so the key belongs
				// immediately before it.
				insertAt = i
			}
			inTarget = name == table
			if inTarget {
				sawTarget = true
			}
			continue
		}

		if inTarget && matchesKey(trimmed, key) {
			lines[i] = assignment
			return []byte(joinLines(lines)), nil
		}
	}

	if !sawTarget {
		// Table absent: append a fresh one.
		out := strings.TrimRight(joinLines(lines), "\n")
		if out != "" {
			out += "\n"
		}
		return []byte(fmt.Sprintf("%s\n[%s]\n%s\n", out, table, assignment)), nil
	}

	if insertAt < 0 {
		insertAt = len(lines)
	}
	lines = append(lines[:insertAt], append([]string{assignment}, lines[insertAt:]...)...)
	return []byte(joinLines(lines)), nil
}

// isTableHeader reports whether a trimmed line is a `[name]` header. Arrays of
// tables (`[[name]]`) are deliberately not treated as headers — they are out of
// scope, and mishandling one is worse than ignoring it.
func isTableHeader(trimmed string) bool {
	return strings.HasPrefix(trimmed, "[") &&
		strings.HasSuffix(trimmed, "]") &&
		!strings.HasPrefix(trimmed, "[[")
}

// matchesKey reports whether a trimmed line assigns key. Comments never match:
// a commented-out setting is not the live one, and uncommenting it by accident
// would change behaviour the user did not ask to change.
func matchesKey(trimmed, key string) bool {
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	eq := strings.IndexByte(trimmed, '=')
	if eq < 0 {
		return false
	}
	return strings.TrimSpace(trimmed[:eq]) == key
}

// quote renders value as a TOML basic string, escaping what would otherwise
// terminate it early.
func quote(value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return `"` + r.Replace(value) + `"`
}

// splitLines splits on \n, dropping the empty element a trailing newline
// produces so joinLines can restore it without the file growing a blank line
// on every edit.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
