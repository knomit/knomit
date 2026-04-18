package hal

import (
	"encoding/json"
	"testing"
)

func TestLink_MarshalJSON_PlainHref(t *testing.T) {
	l := Link{Href: "/api/v1/repos/alpha"}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"href":"/api/v1/repos/alpha"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestLink_MarshalJSON_Templated(t *testing.T) {
	l := Link{Href: "/api/v1/search{?q,limit}", Templated: true}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"href":"/api/v1/search{?q,limit}","templated":true}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestLinkMap_OmitsNilEntries(t *testing.T) {
	m := LinkMap{
		"self": Link{Href: "/a"},
		"next": Link{}, // zero — must be omitted
	}
	b, err := json.Marshal(m.Clean())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"self":{"href":"/a"}}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}
