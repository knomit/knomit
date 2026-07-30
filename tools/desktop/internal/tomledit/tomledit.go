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
// top-level keys and simple `[table]` headers, with string values, optionally
// annotated with a trailing `# comment` — the house style used throughout
// this repo's own TOML (see tools/drone/drone.example.toml). Arrays of
// tables, dotted keys (`a.b = 1`), quoted keys (`"a" = 1`) and inline tables
// (`log = { level = "info" }`) are all out of scope, and they fail in two
// different ways.
//
// Arrays of tables, dotted keys and quoted keys are not RECOGNISED as the key
// SetString was asked to touch, so instead of updating them in place it
// inserts a same-named key elsewhere in the file. That is not merely "appended
// instead of updated" — depending on the shape, the result can be a knomit.toml
// that no longer parses at all (for example, a dotted root key and a freshly
// appended plain key of the same name collide, and BurntSushi rejects the file
// with "Key ... has already been defined").
//
// An inline table — `log = { level = "info" }` — can fail EITHER way, and one
// of the two is silent. Asked for that key directly (table "", key "log"), the
// line IS matched, because it is a plain `key =` assignment as far as
// matchesKey is concerned: SetString overwrites the whole line with a scalar,
// destroying every key nested inside it, and the result still parses, so
// nothing downstream ever complains. Asked instead for something INSIDE it
// (table "log", key "level" — what the settings dialog does), the inline table
// is not seen as a table header at all, so a fresh `[log]` is appended and the
// file stops parsing with "Key 'log' has already been defined". Both are
// pinned by TestSetStringClobbersAnInlineTable and
// TestSetStringCollidesWithAnInlineTableOfTheSameName. Do not reach for
// SetString on a file that may hold one.
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
		// A line may end in "\r" (the file uses CRLF): keep that ending
		// intact on whatever we write back to this line, so editing one line
		// of a CRLF file doesn't leave it with a lone LF amid CRLF siblings.
		cr := ""
		withoutCR := line
		if strings.HasSuffix(withoutCR, "\r") {
			cr = "\r"
			withoutCR = withoutCR[:len(withoutCR)-1]
		}

		// A trailing "# comment" must not affect whether a line is seen as a
		// table header or a key assignment — knomit.toml itself is written in
		// that annotated style (see tools/drone/drone.example.toml) — but it
		// must survive when the line is rewritten.
		code, comment := splitComment(withoutCR)
		trimmed := strings.TrimSpace(code)

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
			lines[i] = withComment(assignment, comment) + cr
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

// matchesKey reports whether a trimmed line (with any trailing comment
// already stripped by splitComment) assigns key. A commented-out setting
// arrives here as an empty trimmed string — its "#" and everything after it
// were removed upstream — so it can never match; a commented-out setting is
// not the live one, and uncommenting it by accident would change behaviour
// the user did not ask to change. The HasPrefix check below is therefore
// belt-and-braces, not the primary defence.
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

// splitComment splits a line (with any trailing "\r" already removed) into
// its meaningful code and a trailing "# ..." comment, so a header or
// assignment can be recognised even when annotated — e.g. `[log] # logging
// settings` or `level = "info"  # trace | debug | info | warn | error`, the
// latter the exact style used by tools/drone/drone.example.toml. A '#' inside
// a basic ("...") or literal ('...') TOML string is not a comment marker and
// does not split the line there.
func splitComment(line string) (code, comment string) {
	inBasic := false
	inLiteral := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case inBasic:
			if c == '\\' {
				i++ // an escaped character (including an escaped quote) can't end the string
			} else if c == '"' {
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '#':
			return line[:i], line[i:]
		}
	}
	return line, ""
}

// withComment re-attaches a trailing comment (including its leading "#") to a
// freshly written assignment, so annotating a value doesn't cost the
// annotation the next time the dialog changes it.
func withComment(assignment, comment string) string {
	if comment == "" {
		return assignment
	}
	return assignment + "  " + comment
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
