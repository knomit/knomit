package proto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadLineResynchronisesAfterAnOversizedLine is the reason ReadLine exists
// instead of bufio.Scanner. Scanner STOPS on a token past its buffer, which
// turns one bad line into a permanently dead channel — the agent would simply
// never answer again, with no error on either side. ReadLine must consume the
// offending line through its newline and carry on.
func TestReadLineResynchronisesAfterAnOversizedLine(t *testing.T) {
	const max = 64
	var buf bytes.Buffer
	buf.WriteString("first\n")
	buf.WriteString(strings.Repeat("x", max*4) + "\n")
	buf.WriteString("third\n")
	br := bufio.NewReader(&buf)

	line, err := ReadLine(br, max)
	if err != nil || string(line) != "first\n" {
		t.Fatalf("first read = (%q, %v)", line, err)
	}

	if _, err := ReadLine(br, max); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("oversized read = %v, want ErrLineTooLong", err)
	}

	line, err = ReadLine(br, max)
	if err != nil || string(line) != "third\n" {
		t.Fatalf("read after the oversized line = (%q, %v); the reader did not resynchronise", line, err)
	}
}

// TestReadLineReportsEOF: a clean end of stream must be io.EOF, because that is
// the agent's shutdown signal and the client's "the child died" signal.
func TestReadLineReportsEOF(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("only\n"))
	if _, err := ReadLine(br, 1024); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := ReadLine(br, 1024); !errors.Is(err, io.EOF) {
		t.Fatalf("read at end = %v, want io.EOF", err)
	}
}

// TestReadLineReturnsAFinalUnterminatedLine: a child killed mid-write can leave
// a partial line. It is handed back rather than dropped, and the caller's JSON
// decode is what rejects it.
func TestReadLineReturnsAFinalUnterminatedLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("tail with no newline"))
	line, err := ReadLine(br, 1024)
	if err != nil {
		t.Fatalf("read = %v", err)
	}
	if string(line) != "tail with no newline" {
		t.Fatalf("line = %q", line)
	}
	if _, err := ReadLine(br, 1024); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the tail = %v, want io.EOF", err)
	}
}

// TestPeekIDRecoversAnIDFromABrokenLine: the id is what lets a malformed
// request be answered rather than left to time out, and a decoder gives nothing
// for a line that does not parse.
func TestPeekIDRecoversAnIDFromABrokenLine(t *testing.T) {
	cases := map[string]uint64{
		`{"id":42,"method":"status"}`: 42,
		`{"id":42,"method":`:          42, // truncated
		`{"id": 7 , "method":"x",,,}`: 7,  // malformed after the id
		`{"method":"status"}`:         0,
		`garbage`:                     0,
		`{"id":"not-a-number"}`:       0,
		`{"id":18446744073709551615}`: 18446744073709551615,
		// Valid JSON goes through the decoder, so a nested id cannot shadow the
		// real one.
		`{"nested":{"id":5},"id":6}`: 6,
		// Broken JSON falls back to the pattern, which takes the FIRST id it
		// sees. Nothing on this wire nests an id, so a heuristic is the right
		// trade against leaving a caller to time out.
		`{"nested":{"id":5},"id":6`:  5,
		`{"id":0,"method":"status"}`: 0,
	}
	for line, want := range cases {
		if got := PeekID([]byte(line)); got != want {
			t.Errorf("PeekID(%s) = %d, want %d", line, got, want)
		}
	}
}

// TestWriteLineIsAllOrNothing: a value that cannot be marshalled must leave the
// stream untouched. A half-written line would desynchronise the channel for
// every request after it.
func TestWriteLineIsAllOrNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteLine(&buf, map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("WriteLine accepted an unmarshalable value")
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteLine wrote %q before failing", buf.String())
	}

	if err := WriteLine(&buf, Response{ID: 3, OK: true}); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("WriteLine did not terminate the line: %q", buf.String())
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 3 || !resp.OK {
		t.Errorf("round trip = %+v", resp)
	}
}
