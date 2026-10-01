package fact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefIdentity(t *testing.T) {
	const local = "3ec012f5b4d2"
	h := func(c string) string { return strings.Repeat(c, 40) }
	src := func(commit, blob, frag string) string {
		return "src://aaaaaaaaaaaa/internal/x.go@" + commit + ":" + blob + frag
	}
	same := [][2]string{
		{"kb/x/y.md", "kb://" + local + "/kb/x/y.md"},
		{src(h("1"), h("a"), ""), src(h("2"), h("a"), "")},
		{src(h("1"), h("a"), ""), src(h("1"), h("a"), "#L3-L9")},
		{"https://Example.ORG/p", "https://example.org/p/"},
		{"https://example.org/p", "https://example.org/p#frag"},
	}
	for _, p := range same {
		require.Equal(t, RefIdentity(p[0], local), RefIdentity(p[1], local), "%v", p)
	}
	diff := [][2]string{
		{src(h("1"), h("a"), ""), src(h("1"), h("b"), "")},
		{"https://example.org/p?a=1", "https://example.org/p?a=2"},
		{"kb/x/y.md", "kb://bbbbbbbbbbbb/kb/x/y.md"},
		{"kb/x/y.md", "https://example.org/kb/x/y.md"},
		{"src://aaaaaaaaaaaa/x.go@" + h("1"), "src://aaaaaaaaaaaa/x.go@" + h("2")}, // legacy: commit counts
	}
	for _, p := range diff {
		require.NotEqual(t, RefIdentity(p[0], local), RefIdentity(p[1], local), "%v", p)
	}
}

func TestHasNovelRef(t *testing.T) {
	const local = "3ec012f5b4d2"
	self := "kb/t/self.md"
	require.False(t, HasNovelRef(nil, []string{"https://a"}, self, local))
	require.False(t, HasNovelRef([]string{"https://a/"}, []string{"https://a"}, self, local))
	require.True(t, HasNovelRef([]string{"https://a", "https://b"}, []string{"https://a"}, self, local))
	require.False(t, HasNovelRef([]string{"kb://" + local + "/" + self}, nil, self, local), "self-ref is never novel")
	require.False(t, HasNovelRef([]string{self}, nil, self, local))
}
