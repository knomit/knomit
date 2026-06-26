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
