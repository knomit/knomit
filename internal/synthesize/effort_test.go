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
