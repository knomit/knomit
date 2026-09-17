//go:build windows

package embeddings

import (
	"errors"
	"strings"
	"testing"
)

// The version reader has to work against the real System32 DLL — a fixture
// would only prove the parsing, not that the file is where we look for it.
// Any machine that can build this package has the runtime installed, since
// onnxruntime.dll links it.
func TestVCRuntimeVersionReadsTheInstalledRuntime(t *testing.T) {
	major, minor, ok := vcRuntimeVersion()
	if !ok {
		t.Fatal("could not read msvcp140.dll's version; the redistributable should be present on any machine that builds this package")
	}
	if major != 14 {
		t.Errorf("major = %d, want 14 — the 2015-2022 redistributable is versioned 14.x", major)
	}
	t.Logf("installed VC++ runtime: %d.%d", major, minor)
	_ = minor
}

func TestAnnotateORTInitErrorPassesNilThrough(t *testing.T) {
	if got := annotateORTInitError(nil); got != nil {
		t.Errorf("annotateORTInitError(nil) = %v, want nil", got)
	}
}

// Whatever the annotation decides, it must not swallow the original failure:
// the DLL message is the only thing that distinguishes "runtime too old" from
// the other reasons a load fails.
func TestAnnotateORTInitErrorKeepsTheCause(t *testing.T) {
	cause := errors.New("A dynamic link library (DLL) initialization routine failed.")
	got := annotateORTInitError(cause)
	if !errors.Is(got, cause) {
		t.Errorf("annotated error does not wrap the cause: %v", got)
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Errorf("annotated error does not quote the cause: %v", got)
	}
}

// The point of reading the version instead of matching the error text: on a
// machine that is already up to date, the advice is wrong and must not appear.
// This asserts against whichever way THIS machine is configured, so it is a
// real check on either side of the boundary rather than a tautology.
func TestAnnotateORTInitErrorAdvisesOnlyWhenTheRuntimeIsOld(t *testing.T) {
	major, minor, ok := vcRuntimeVersion()
	uptodate := ok && (major > minVCRuntimeMajor || (major == minVCRuntimeMajor && minor >= minVCRuntimeMinor))

	msg := annotateORTInitError(errors.New("boom")).Error()
	mentions := strings.Contains(msg, vcRedistURL)

	switch {
	case uptodate && mentions:
		t.Errorf("runtime is %d.%d (>= %d.%d) but the error still tells the user to install the redistributable:\n%s",
			major, minor, minVCRuntimeMajor, minVCRuntimeMinor, msg)
	case !uptodate && !mentions:
		t.Errorf("runtime is too old (%d.%d, ok=%v) but the error does not name the requirement:\n%s",
			major, minor, ok, msg)
	}
}
