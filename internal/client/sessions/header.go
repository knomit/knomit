package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ClientHeader carries a client's self-description on every MCP request.
// Format follows RFC 7239 Forwarded: `key=value;key=value`, values quoted
// with backslash escapes when they contain ';', '=', '"' or whitespace.
// Unknown keys are ignored so a newer bridge can add fields without a server
// change, and a newer server can add columns without breaking old bridges.
const ClientHeader = "X-Knomit-Client"

// MaxFieldLen caps every client-supplied value — the same bound
// internal/web/observability.go applies to logged request fields, for the
// same reason: an unbounded value would ride into a DB column.
const MaxFieldLen = 256

const truncationSuffix = "...[truncated]"

// BridgeInfo is the parsed X-Knomit-Client header. Every field is
// SELF-DECLARED and unverified: this is correlation, not identity.
type BridgeInfo struct {
	InstanceID string // key "id"
	Transport  string // key "transport": "stdio" or "http"
	PID        int    // key "pid"
	ParentPID  int    // key "ppid"
	ParentApp  string // key "parent"
	Host       string // key "host"
	User       string // key "user"
	Cwd        string // key "cwd"
	Branch     string // key "branch"
	Version    string // key "v"
}

// Encode renders the header value. Field order is fixed so the value is
// stable for logs and tests.
func (b BridgeInfo) Encode() string {
	pairs := []struct{ k, v string }{
		{"id", b.InstanceID},
		{"transport", b.Transport},
		{"pid", strconv.Itoa(b.PID)},
		{"ppid", strconv.Itoa(b.ParentPID)},
		{"parent", b.ParentApp},
		{"host", b.Host},
		{"user", b.User},
		{"cwd", b.Cwd},
		{"branch", b.Branch},
		{"v", b.Version},
	}
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(p.k)
		sb.WriteByte('=')
		sb.WriteString(quoteIfNeeded(p.v))
	}
	return sb.String()
}

func quoteIfNeeded(v string) string {
	if v != "" && !strings.ContainsAny(v, ";=\" \t\\") {
		return v
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range v {
		if r == '"' || r == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	sb.WriteByte('"')
	return sb.String()
}

// ParseClientHeader parses a header value. An empty string is an error (the
// caller treats "absent" and "unparseable" the same way: no declared info).
func ParseClientHeader(s string) (BridgeInfo, error) {
	var out BridgeInfo
	if strings.TrimSpace(s) == "" {
		return out, fmt.Errorf("empty %s header", ClientHeader)
	}
	i := 0
	for i < len(s) {
		// key
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return BridgeInfo{}, fmt.Errorf("pair without '=' at offset %d", i)
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		// value
		var val string
		if i < len(s) && s[i] == '"' {
			i++
			var sb strings.Builder
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					sb.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				sb.WriteByte(c)
				i++
			}
			if !closed {
				return BridgeInfo{}, fmt.Errorf("unterminated quote in %q", key)
			}
			val = sb.String()
			// skip to next ';'
			if j := strings.IndexByte(s[i:], ';'); j >= 0 {
				i += j + 1
			} else {
				i = len(s)
			}
		} else {
			j := strings.IndexByte(s[i:], ';')
			if j < 0 {
				val = s[i:]
				i = len(s)
			} else {
				val = s[i : i+j]
				i += j + 1
			}
			val = strings.TrimSpace(val)
		}
		val = Cap(val)
		if err := out.set(key, val); err != nil {
			return BridgeInfo{}, err
		}
	}
	return out, nil
}

func (b *BridgeInfo) set(key, val string) error {
	switch key {
	case "id":
		b.InstanceID = val
	case "transport":
		b.Transport = val
	case "parent":
		b.ParentApp = val
	case "host":
		b.Host = val
	case "user":
		b.User = val
	case "cwd":
		b.Cwd = val
	case "branch":
		b.Branch = val
	case "v":
		b.Version = val
	case "pid", "ppid":
		if val == "" {
			return nil
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if key == "pid" {
			b.PID = n
		} else {
			b.ParentPID = n
		}
	default:
		// Unknown key: ignored by design (forward compatibility).
	}
	return nil
}

// Cap truncates a client-supplied value to MaxFieldLen bytes, backing off a
// partial rune, and marks the cut.
func Cap(s string) string {
	if len(s) <= MaxFieldLen {
		return s
	}
	cut := s[:MaxFieldLen]
	for i := 0; i < utf8.UTFMax-1 && len(cut) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + truncationSuffix
}

// DeriveInstanceID hashes the parts with a NUL separator (so field
// boundaries cannot collide) and returns the first 16 hex of SHA-256.
// Used by the bridge (host, user, cwd, parent, pid, start time) and by the
// server for direct HTTP callers (remote ip, user-agent, client name/version).
func DeriveInstanceID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])[:16]
}
