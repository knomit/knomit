package hal

import (
	"encoding/json"
	"strings"
	"testing"
)

type stubItem struct {
	Name  string  `json:"name"`
	Links LinkMap `json:"_links"`
}

func TestCollectionView_Marshal_HasCountAndEmbedded(t *testing.T) {
	c := CollectionView[stubItem]{
		Count: 2,
		Links: LinkMap{
			"self": {Href: "/api/v1-new/repos"},
		},
		Embedded: map[string][]stubItem{
			"repos": {
				{Name: "alpha", Links: LinkMap{"self": {Href: "/api/v1-new/repos/alpha"}}},
				{Name: "beta", Links: LinkMap{"self": {Href: "/api/v1-new/repos/beta"}}},
			},
		},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"count":2`,
		`"_links":{"self":{"href":"/api/v1-new/repos"}}`,
		`"_embedded":{"repos":[`,
		`"name":"alpha"`,
		`"name":"beta"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func TestCollectionView_Marshal_EmptyEmbedded_EmitsEmptyArray(t *testing.T) {
	c := CollectionView[stubItem]{
		Count:    0,
		Links:    LinkMap{"self": {Href: "/a"}},
		Embedded: map[string][]stubItem{"repos": {}},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"_embedded":{"repos":[]}`) {
		t.Errorf("expected empty array, got %s", b)
	}
}
