package synthesize

import "testing"

func TestEffortValidate(t *testing.T) {
	for _, e := range []Effort{EffortNormal, EffortMedium, EffortHigh} {
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", e, err)
		}
	}
	if err := Effort("turbo").Validate(); err == nil {
		t.Error("Validate(\"turbo\") = nil, want error")
	}
	if err := Effort("").Validate(); err == nil {
		t.Error("Validate(\"\") = nil, want error")
	}
}

func TestEffortDefault(t *testing.T) {
	if DefaultEffort != EffortNormal {
		t.Errorf("DefaultEffort = %q, want %q", DefaultEffort, EffortNormal)
	}
}

func TestEffortDiscovers(t *testing.T) {
	cases := []struct {
		e    Effort
		want bool
	}{
		{EffortNormal, false},
		{EffortMedium, true},
		{EffortHigh, true},
	}
	for _, tc := range cases {
		if got := tc.e.Discovers(); got != tc.want {
			t.Errorf("Discovers(%q) = %v, want %v", tc.e, got, tc.want)
		}
	}
}

func TestNormalizeEffort(t *testing.T) {
	if got := NormalizeEffort(""); got != DefaultEffort {
		t.Errorf("NormalizeEffort(\"\") = %q, want %q", got, DefaultEffort)
	}
	if got := NormalizeEffort(EffortHigh); got != EffortHigh {
		t.Errorf("NormalizeEffort(high) = %q, want high", got)
	}
}

// TestScopeFilterMatches pins the single definition of scope membership used by
// both the review and hypothesize incremental seed paths. Union semantics:
// empty filter matches everything; a non-empty filter matches a fact that
// touches at least one requested domain OR entity. A token listed as a domain
// must not be satisfied by the same string appearing only as an entity.
func TestScopeFilterMatches(t *testing.T) {
	empty := ScopeFilter{}
	if !empty.Matches([]string{"anything"}, nil) {
		t.Error("empty filter must match a tagged fact")
	}
	if !empty.Matches(nil, nil) {
		t.Error("empty filter must match an untagged fact")
	}

	dom := ScopeFilter{Domain: []string{"auth"}}
	if !dom.Matches([]string{"auth", "billing"}, nil) {
		t.Error("domain filter must match a fact carrying that domain")
	}
	if dom.Matches([]string{"billing"}, []string{"auth"}) {
		t.Error("domain filter must NOT match when 'auth' appears only as an entity")
	}

	ent := ScopeFilter{Entities: []string{"alice"}}
	if !ent.Matches(nil, []string{"alice"}) {
		t.Error("entity filter must match a fact carrying that entity")
	}
	if ent.Matches([]string{"alice"}, []string{"bob"}) {
		t.Error("entity filter must NOT match a non-listed entity")
	}

	both := ScopeFilter{Domain: []string{"auth"}, Entities: []string{"alice"}}
	if !both.Matches(nil, []string{"alice"}) {
		t.Error("union filter must match on entity alone")
	}
	if !both.Matches([]string{"auth"}, nil) {
		t.Error("union filter must match on domain alone")
	}
	if both.Matches([]string{"ops"}, []string{"bob"}) {
		t.Error("union filter must NOT match when neither axis matches")
	}
}
