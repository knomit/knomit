package fact

import (
	"strings"
	"testing"
)

func TestAppendUnique(t *testing.T) {
	t.Run("appends when absent", func(t *testing.T) {
		result := AppendUnique([]string{"a", "b"}, "c")
		if len(result) != 3 || result[2] != "c" {
			t.Fatalf("got %v", result)
		}
	})
	t.Run("no-op when present", func(t *testing.T) {
		slice := []string{"a", "b"}
		result := AppendUnique(slice, "a")
		if len(result) != 2 {
			t.Fatalf("expected length 2, got %v", result)
		}
	})
}

func TestNormalizePath(t *testing.T) {
	root := "kb"
	tests := []struct {
		input string
		want  string
	}{
		{"topic/foo", "kb/topic/foo.md"},
		{"kb/topic/foo", "kb/topic/foo.md"},
		{"kb/topic/foo.md", "kb/topic/foo.md"},
		{"topic/foo.md", "kb/topic/foo.md"},
	}
	for _, tc := range tests {
		got := NormalizePath(root, tc.input)
		if got != tc.want {
			t.Errorf("NormalizePath(%q, %q) = %q, want %q", root, tc.input, got, tc.want)
		}
	}
}

func TestUnionStrings(t *testing.T) {
	got := UnionStrings([]string{"a", "b"}, []string{"b", "c"})
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
