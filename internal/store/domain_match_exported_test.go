package store

import "testing"

func TestDomainTagMatches(t *testing.T) {
	cases := []struct {
		factTag, queryTag string
		want              bool
	}{
		{"store", "store", true},                 // exact
		{"store", "Store", true},                 // case-insensitive
		{"store/sqlite", "store", true},          // query is ancestor (descendant-or-equal)
		{"store/sqlite", "sqlite", true},         // token containment on a segment
		{"remote sync", "remote-sync", true},     // de-hyphenize
		{"migration", "migrations", true},        // stem (plural query)
		{"store", "store/sqlite", false},         // query more specific than fact
		{"store", "embeddings", false},           // unrelated
	}
	for _, c := range cases {
		if got := DomainTagMatches(c.factTag, c.queryTag); got != c.want {
			t.Errorf("DomainTagMatches(%q,%q)=%v want %v", c.factTag, c.queryTag, got, c.want)
		}
	}
}

func TestEntityTagMatches(t *testing.T) {
	if !EntityTagMatches("Anthropic", "anthropic") {
		t.Error("entity match should be case-insensitive")
	}
	if EntityTagMatches("OpenAI", "Google") {
		t.Error("distinct entities must not match")
	}
}
