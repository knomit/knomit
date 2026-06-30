package crashdump

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRingWriterRetainsLastN(t *testing.T) {
	w := NewRingWriter(2)
	fmt.Fprintln(w, "line1")
	fmt.Fprintln(w, "line2")
	fmt.Fprintln(w, "line3")

	got := w.Lines()
	want := []string{"line2", "line3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
}

func TestRingWriterSplitsMultilineWrite(t *testing.T) {
	w := NewRingWriter(5)
	// A single Write carrying several newline-separated records.
	if _, err := w.Write([]byte("a\nb\nc\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := w.Lines()
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
}

func TestRingWriterWriteReportsFullLength(t *testing.T) {
	// io.Writer contract: Write must report it consumed every byte, or
	// callers (zerolog's MultiLevelWriter) treat it as a short-write error.
	w := NewRingWriter(2)
	p := []byte("hello\n")
	n, err := w.Write(p)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p) {
		t.Fatalf("Write returned n=%d, want %d", n, len(p))
	}
}

func TestRingWriterEmpty(t *testing.T) {
	w := NewRingWriter(3)
	if got := w.Lines(); len(got) != 0 {
		t.Fatalf("Lines() on empty = %v, want empty", got)
	}
}
