package crashdump

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEndUnlessPanicking_ClearsMarkerOnCleanReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.marker")
	m := NewMarker(path)
	if _, _, err := m.Begin(time.Now()); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	func() { defer m.EndUnlessPanicking() }() // clean return → marker cleared

	crashed, _, err := NewMarker(path).Begin(time.Now())
	if err != nil {
		t.Fatalf("Begin after clean return: %v", err)
	}
	if crashed {
		t.Fatal("EndUnlessPanicking did not clear the marker on a clean return")
	}
}

func TestEndUnlessPanicking_PreservesMarkerAndRepanicsOnPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.marker")
	m := NewMarker(path)
	if _, _, err := m.Begin(time.Now()); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Mirror serve's defer wiring: a panic unwinds through EndUnlessPanicking on
	// its way to a crash-path recover (here, the outer recover stands in for
	// reporter.Guard). The marker must survive so the next boot detects the
	// unclean exit, and the panic must keep propagating.
	repanicked := false
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				repanicked = true
			}
		}()
		defer m.EndUnlessPanicking()
		panic("boom")
	}()

	if !repanicked {
		t.Fatal("EndUnlessPanicking swallowed the panic; it must re-raise")
	}

	crashed, _, err := NewMarker(path).Begin(time.Now())
	if err != nil {
		t.Fatalf("Begin after panic: %v", err)
	}
	if !crashed {
		t.Fatal("marker was erased during a panic unwind; crash-loop signal lost")
	}
}

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
