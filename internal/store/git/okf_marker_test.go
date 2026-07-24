package git

import "testing"

func newTestStorer(t *testing.T) *Storer {
	s, err := NewMemoryStorer()
	if err != nil {
		t.Fatalf("NewMemoryStorer: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
	})
	return s
}

func TestOKFMarker_RoundTrip(t *testing.T) {
	s := newTestStorer(t)
	if v, err := s.OKFMarkerGet("main"); err != nil || v != "" {
		t.Fatalf("empty marker: got (%q,%v), want (\"\",nil)", v, err)
	}
	if err := s.OKFMarkerSet("main", "abc\n1\ndef"); err != nil {
		t.Fatal(err)
	}
	v, err := s.OKFMarkerGet("main")
	if err != nil || v != "abc\n1\ndef" {
		t.Fatalf("got (%q,%v)", v, err)
	}
}
