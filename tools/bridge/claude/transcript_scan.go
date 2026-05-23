package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// scanTranscript invokes onMsg for the last maxMessages user/assistant
// messages in path, in chronological order (oldest of those N first, most
// recent last). onMsg returns false to stop scanning early.
//
// The file is tail-read in 4KB chunks (seeking backwards) so we only
// materialise the last maxMessages dialogue messages, not the full
// transcript. Once collected, we iterate them forward so callers can use
// prevRole gates against the *previous in dialogue* message.
func scanTranscript(path string, maxMessages int, onMsg func(role, text string) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	type msg struct{ role, text string }
	buf := make([]msg, 0, maxMessages)

	for line, err := range tailLines(f) {
		if err != nil {
			return err
		}
		role, text, ok := parseTranscriptLine(line)
		if !ok {
			continue
		}
		buf = append(buf, msg{role, text})
		if len(buf) >= maxMessages {
			break
		}
	}
	// buf is tail-first (most recent appended first). Iterate in reverse
	// to deliver in chronological order.
	for i := len(buf) - 1; i >= 0; i-- {
		if !onMsg(buf[i].role, buf[i].text) {
			return nil
		}
	}
	return nil
}

// tailLines yields lines from f in reverse order (last line first) by
// seeking backwards in 4KB chunks and splitting on newlines.
func tailLines(f *os.File) func(yield func(string, error) bool) {
	return func(yield func(string, error) bool) {
		const chunkSize = 4096

		info, err := f.Stat()
		if err != nil {
			yield("", fmt.Errorf("stat transcript: %w", err))
			return
		}
		size := info.Size()
		if size == 0 {
			return
		}

		pos := size
		var tail []byte // bytes from earlier seek not yet split off

		for pos > 0 {
			readSize := int64(chunkSize)
			if pos < readSize {
				readSize = pos
			}
			pos -= readSize

			buf := make([]byte, readSize)
			if _, err := f.ReadAt(buf, pos); err != nil && err != io.EOF {
				yield("", fmt.Errorf("read transcript: %w", err))
				return
			}
			buf = append(buf, tail...)

			// Split off all complete lines from the right.
			for {
				i := lastIndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := string(buf[i+1:])
				buf = buf[:i]
				if line == "" {
					continue
				}
				if !yield(line, nil) {
					return
				}
			}
			// Remainder has no newlines yet; prepend on next iteration.
			tail = buf
		}
		if len(tail) > 0 {
			yield(string(tail), nil)
		}
	}
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// commandStubRE matches CC's slash-command stub user messages
// (<command-message>...</command-message><command-name>/...</command-name>).
// These are not dialogue and must be skipped by the scanner.
var commandStubRE = regexp.MustCompile(`(?s)^\s*<command-message>.*?</command-message>\s*<command-name>/`)

// parseTranscriptLine extracts (role, text) from one JSONL line. Returns
// ok=false if the line is not a user/assistant message with non-empty
// dialogue text, or if it's a command stub.
func parseTranscriptLine(line string) (role, text string, ok bool) {
	var entry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return "", "", false
	}
	if entry.Type != "user" && entry.Type != "assistant" {
		return "", "", false
	}
	text = extractContentText(entry.Message)
	if text == "" {
		return "", "", false
	}
	if commandStubRE.MatchString(text) {
		return "", "", false
	}
	return entry.Type, text, true
}

// extractContentText handles message.content's variable shape: string OR
// array of parts. Only type:"text" parts are kept; thinking, tool_use,
// and tool_result parts are dropped.
func extractContentText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}
