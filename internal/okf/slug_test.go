// internal/okf/slug_test.go
package okf

import "testing"

func TestSlug(t *testing.T) {
	cases := []struct {
		name     string
		title    string
		category string
		uuid8    string
		want     string
	}{
		{"simple", "Reject replica mounts", "reject-replica-mounts", "a8e4391d",
			"reject-replica-mounts-a8e4391d.md"},
		{"punctuation collapses", "Foo:  bar / baz!!", "cat", "0000aaaa",
			"foo-bar-baz-0000aaaa.md"},
		{"accents fold to ascii", "Café résumé", "cat", "11112222",
			"cafe-resume-11112222.md"},
		{"leading/trailing separators trimmed", "  --Hello--  ", "cat", "33334444",
			"hello-33334444.md"},
		{"truncate at hyphen boundary <=60",
			"this is a very long title that keeps going well past the sixty character limit for names",
			"cat", "55556666",
			"this-is-a-very-long-title-that-keeps-going-well-past-the-55556666.md"},
		{"first word longer than 60 hard-cut",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaXbbb",
			"cat", "77778888",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-77778888.md"},
		{"empty slug falls back to category", "、。！", "reject-replica-mounts", "99990000",
			"reject-replica-mounts-99990000.md"},
		{"empty slug and empty category falls back to uuid", "、。！", "", "aaaabbbb",
			"aaaabbbb.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Slug(c.title, c.category, c.uuid8); got != c.want {
				t.Fatalf("Slug(%q,%q,%q) = %q, want %q", c.title, c.category, c.uuid8, got, c.want)
			}
		})
	}
}
