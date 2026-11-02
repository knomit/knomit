package remotesync

import "testing"

func TestSetOriginIfURL(t *testing.T) {
	ri := &Index{}
	if ri.SetOriginIfURL("https://a/x", &Origin{URL: "https://a/x", AuthToken: "new"}) {
		t.Fatal("applied with no origin injected: a removed origin would come back")
	}
	if ri.getOrigin() != nil {
		t.Fatal("origin resurrected")
	}

	ri.SetOrigin(&Origin{URL: "https://b/x", AuthToken: "b"})
	if ri.SetOriginIfURL("https://a/x", &Origin{URL: "https://a/x", AuthToken: "new"}) {
		t.Fatal("applied over another URL")
	}
	if got := ri.getOrigin(); got.URL != "https://b/x" || got.AuthToken != "b" {
		t.Fatalf("origin changed: %+v", got)
	}

	if !ri.SetOriginIfURL("https://b/x", &Origin{URL: "https://b/x", AuthToken: "new"}) {
		t.Fatal("refused on the expected URL")
	}
	if got := ri.getOrigin(); got.AuthToken != "new" {
		t.Fatalf("not applied: %+v", got)
	}
}
