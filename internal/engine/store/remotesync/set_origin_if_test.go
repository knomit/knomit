package remotesync

import "testing"

func TestSetOriginIf(t *testing.T) {
	ri := &Index{}
	a := Origin{URL: "https://a/x", AuthMethod: "token", AuthToken: "a"}
	if ri.SetOriginIf(a, &Origin{URL: "https://a/x", AuthToken: "new"}) {
		t.Fatal("applied with no origin injected: a removed origin would come back")
	}
	if ri.InjectedOrigin() != nil {
		t.Fatal("origin resurrected")
	}

	ri.SetOrigin(&Origin{URL: "https://b/x", AuthToken: "b"})
	if ri.SetOriginIf(a, &Origin{URL: "https://a/x", AuthToken: "new"}) {
		t.Fatal("applied over another URL")
	}
	// The same URL with another credential (a re-attach) is not a match.
	ri.SetOrigin(&Origin{URL: "https://a/x", AuthMethod: "basic", AuthToken: "u:p"})
	if ri.SetOriginIf(a, &Origin{URL: "https://a/x", AuthToken: "new"}) {
		t.Fatal("applied over a re-attached credential")
	}
	if got := ri.InjectedOrigin(); got.AuthToken != "u:p" {
		t.Fatalf("origin changed: %+v", got)
	}

	ri.SetOrigin(&a)
	if !ri.SetOriginIf(a, &Origin{URL: "https://a/x", AuthMethod: "token", AuthToken: "new"}) {
		t.Fatal("refused on the expected origin")
	}
	if got := ri.InjectedOrigin(); got.AuthToken != "new" {
		t.Fatalf("not applied: %+v", got)
	}
}
