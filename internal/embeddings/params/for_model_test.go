package params

import "testing"

// TestForModelFindsTheShippedModel is the reason this accessor exists: the
// cgo-free half of the contract knew WHICH model ships (DefaultModelID) but not
// its geometry, which lived inline in the cgo parent's descriptor. Anything
// needing the shipped band without linking ONNX had to retype the numbers.
func TestForModelFindsTheShippedModel(t *testing.T) {
	th, ok := ForModel(DefaultModelID)
	if !ok {
		t.Fatalf("ForModel(%q) not found; the shipped model must carry calibrated thresholds", DefaultModelID)
	}
	if th == Defaults() {
		t.Fatalf("ForModel(%q) returned the nomic fallback; the shipped model has its own calibration", DefaultModelID)
	}
	if th.ReflectNovelty >= th.Dedup {
		t.Errorf("ReflectNovelty %v must sit below Dedup %v", th.ReflectNovelty, th.Dedup)
	}
}

// TestForModelReportsAbsenceRatherThanDefaulting is the whole point of the bool.
// A silent fallback to Defaults() for an unknown id is how a typo'd or
// newly-added model id would be judged against NOMIC geometry with nothing red
// — the same defect class as bridge_motif's "UNKNOWN, not defaulted" (M-5), and
// the reason kb/invariants/integrations/hooks/guards-fail-closed requires a
// guard to distinguish absent from zero rather than proceed on a zero value.
func TestForModelReportsAbsenceRatherThanDefaulting(t *testing.T) {
	th, ok := ForModel("no-such-model")
	if ok {
		t.Errorf("ForModel(unknown) reported found")
	}
	if th != (Thresholds{}) {
		t.Errorf("ForModel(unknown) = %+v, want the zero value so a caller cannot mistake it for a band", th)
	}
}

// TestForModelNomicIsPresentNotInferred is the trap the bool must survive.
// nomic-v1.5's descriptor IS params.Defaults(), so implementing presence as
// `th == Defaults()` would report a real, registered model as missing. Presence
// must be a map-key probe, exactly as requireResponseKey probes for key presence
// rather than for a non-empty value.
func TestForModelNomicIsPresentNotInferred(t *testing.T) {
	th, ok := ForModel(nomicModelID)
	if !ok {
		t.Fatalf("ForModel(%q) not found; a model whose thresholds equal Defaults() is still a registered model", nomicModelID)
	}
	if th != Defaults() {
		t.Errorf("ForModel(%q) = %+v, want Defaults()", nomicModelID, th)
	}
}

// Defaults() must stay the nomic alias rather than a second copy of the same
// numbers, so there is one place a re-sweep of nomic has to land.
func TestDefaultsIsTheNomicEntry(t *testing.T) {
	th, ok := ForModel(nomicModelID)
	if !ok || th != Defaults() {
		t.Errorf("Defaults() must be the %q entry, not an independent literal", nomicModelID)
	}
}
