package fact

import "testing"

func TestOriginValidate(t *testing.T) {
	for _, o := range []Origin{Authored, Distilled, Discovered} {
		if err := o.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", o, err)
		}
	}
	if err := Origin("bogus").Validate(); err == nil {
		t.Error("Validate(\"bogus\") = nil, want error")
	}
	if DefaultOrigin != Authored {
		t.Errorf("DefaultOrigin = %q, want %q", DefaultOrigin, Authored)
	}
}

func TestOriginValidateForType(t *testing.T) {
	cases := []struct {
		origin Origin
		typ    Type
		ok     bool
	}{
		// authored pairs with anything — including source-transcribed observations.
		{Authored, Observation, true},
		{Authored, Synthesis, true},
		{Authored, Type("policy"), true},
		// distilled: synthesis-pipeline output only.
		{Distilled, Synthesis, true},
		{Distilled, Observation, false},
		{Distilled, Hypothesis, false},
		// discovered: discovery-engine output — forward (synthesis) or backward (hypothesis).
		{Discovered, Synthesis, true},
		{Discovered, Hypothesis, true},
		{Discovered, Observation, false},
		{Discovered, Type("policy"), false},
	}
	for _, c := range cases {
		err := c.origin.ValidateForType(c.typ)
		if c.ok && err != nil {
			t.Errorf("ValidateForType(%q, %q) = %v, want nil", c.origin, c.typ, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateForType(%q, %q) = nil, want error", c.origin, c.typ)
		}
	}
}
