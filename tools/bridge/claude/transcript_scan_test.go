package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestScanTranscript_LastNMessagesChronological(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"user","message":{"content":"first user msg"}}`,
		`{"type":"assistant","message":{"content":"first assistant msg"}}`,
		`{"type":"user","message":{"content":"second user msg"}}`,
		`{"type":"assistant","message":{"content":"second assistant msg"}}`,
	})

	var got []string
	err := scanTranscript(path, 2, func(role, text string) bool {
		got = append(got, role+":"+text)
		return true
	})
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	// Tail-read the last 2 messages, deliver in chronological order:
	// "second user msg" came before "second assistant msg" in the file.
	want := []string{
		"user:second user msg",
		"assistant:second assistant msg",
	}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScanTranscript_EarlyStop(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"user","message":{"content":"msg 1"}}`,
		`{"type":"assistant","message":{"content":"msg 2"}}`,
		`{"type":"user","message":{"content":"msg 3"}}`,
	})

	count := 0
	err := scanTranscript(path, 10, func(role, text string) bool {
		count++
		return count < 2 // stop after 2 callbacks
	})
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (early-stop honoured)", count)
	}
}

func TestScanTranscript_SkipsNonDialogue(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"system","message":{"content":"skip me"}}`,
		`{"type":"ai-title","message":{"content":"skip me too"}}`,
		`{"type":"user","message":{"content":"keep me"}}`,
		`{"type":"assistant","message":{"content":"keep me too"}}`,
	})

	var roles []string
	err := scanTranscript(path, 10, func(role, text string) bool {
		roles = append(roles, role)
		return true
	})
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	want := []string{"user", "assistant"}
	if !equalStrings(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
}

func TestScanTranscript_SkipsCommandStubs(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"user","message":{"content":"<command-message>foo</command-message>\n<command-name>/foo</command-name>"}}`,
		`{"type":"user","message":{"content":"real prompt"}}`,
	})

	var got []string
	err := scanTranscript(path, 10, func(role, text string) bool {
		got = append(got, text)
		return true
	})
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	if len(got) != 1 || got[0] != "real prompt" {
		t.Errorf("got %v, want [\"real prompt\"]", got)
	}
}

func TestScanTranscript_ContentArray(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"assistant","message":{"content":[{"type":"thinking","text":"hidden"},{"type":"text","text":"shown"}]}}`,
	})

	var got string
	err := scanTranscript(path, 10, func(role, text string) bool {
		got = text
		return true
	})
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	if got != "shown" {
		t.Errorf("got %q, want %q", got, "shown")
	}
}

func TestScanTranscript_MissingFile(t *testing.T) {
	err := scanTranscript("/nonexistent/transcript.jsonl", 10, func(role, text string) bool {
		t.Fatal("callback should not fire for missing file")
		return false
	})
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestScanTranscript_LargeFileTailRead(t *testing.T) {
	// Write 1000 lines; only the last 5 should be visited.
	lines := make([]string, 1000)
	for i := range lines {
		lines[i] = `{"type":"user","message":{"content":"msg ` + itoa(i) + `"}}`
	}
	path := writeTranscript(t, lines)

	var got []string
	err := scanTranscript(path, 5, func(role, text string) bool {
		got = append(got, text)
		return true
	})
	if err != nil {
		t.Fatalf("scanTranscript: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d lines, want 5", len(got))
	}
	// Chronological iteration of the last 5 messages:
	// got[0] is the oldest of those 5 (msg 995), got[4] is the newest (msg 999).
	if got[0] != "msg 995" {
		t.Errorf("got[0] = %q, want %q (oldest of last 5)", got[0], "msg 995")
	}
	if got[len(got)-1] != "msg 999" {
		t.Errorf("last (most recent) = %q, want %q", got[len(got)-1], "msg 999")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
