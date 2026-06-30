package crashdump

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerFirstRunNoPriorCrash(t *testing.T) {
	m := NewMarker(filepath.Join(t.TempDir(), "running.marker"))
	crashed, _, err := m.Begin(time.Now())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if crashed {
		t.Fatal("first run reported a prior crash")
	}
}

func TestMarkerDetectsPriorCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.marker")
	prior := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)

	// First process starts but never cleanly ends (simulated crash).
	m1 := NewMarker(path)
	if _, _, err := m1.Begin(prior); err != nil {
		t.Fatalf("first Begin: %v", err)
	}

	// Next process starts and should see the stale marker.
	m2 := NewMarker(path)
	crashed, priorStart, err := m2.Begin(time.Now())
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if !crashed {
		t.Fatal("did not detect the prior unclean exit")
	}
	if !priorStart.Equal(prior) {
		t.Errorf("priorStart = %v, want %v", priorStart, prior)
	}
}

func TestMarkerCleanShutdownClearsMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.marker")

	m1 := NewMarker(path)
	if _, _, err := m1.Begin(time.Now()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := m1.End(); err != nil {
		t.Fatalf("End: %v", err)
	}

	m2 := NewMarker(path)
	crashed, _, err := m2.Begin(time.Now())
	if err != nil {
		t.Fatalf("Begin after clean End: %v", err)
	}
	if crashed {
		t.Fatal("reported a crash after a clean shutdown")
	}
}
