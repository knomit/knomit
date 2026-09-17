//go:build windows

package embeddings

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// vcRedistURL is the Microsoft-hosted evergreen installer for the x64 VC++
// 2015-2022 redistributable.
const vcRedistURL = "https://aka.ms/vs/17/release/vc_redist.x64.exe"

// minVCRuntimeMajor/Minor is the floor the prebuilt onnxruntime binaries have
// required since ORT 1.21.0 ("All the prebuilt Windows packages now require
// VC++ Runtime version >= 14.40"). Below it, loading onnxruntime.dll fails
// with nothing more useful than "A dynamic link library (DLL) initialization
// routine failed."
const (
	minVCRuntimeMajor = 14
	minVCRuntimeMinor = 40
)

// annotateORTInitError adds the VC++ redistributable requirement to an ORT
// initialisation failure, but ONLY when this machine's runtime is actually too
// old — telling someone on 14.44 to install 14.40 sends them after the wrong
// problem.
//
// The check is the installed version, not the text of err. Matching the DLL
// error string would be brittle (it is localised, and the loader reports
// several different messages for the same root cause) and would still be a
// guess about the cause; the runtime version is the thing the requirement is
// actually about.
//
// A runtime we cannot find or cannot read is reported too: a missing
// msvcp140.dll means the redistributable is not installed at all, which is the
// same fix.
func annotateORTInitError(err error) error {
	if err == nil {
		return nil
	}
	major, minor, ok := vcRuntimeVersion()
	if ok && (major > minVCRuntimeMajor || (major == minVCRuntimeMajor && minor >= minVCRuntimeMinor)) {
		return err // new enough; whatever went wrong, this is not it
	}
	found := "not installed"
	if ok {
		found = fmt.Sprintf("%d.%d", major, minor)
	}
	return fmt.Errorf("%w\n\n"+
		"onnxruntime needs the Microsoft Visual C++ 2015-2022 redistributable (x64), "+
		"version %d.%d or newer; this machine has %s.\n"+
		"Install it from %s and start knomit again.",
		err, minVCRuntimeMajor, minVCRuntimeMinor, found, vcRedistURL)
}

// vcRuntimeVersion reads the file version of the installed VC++ runtime.
//
// msvcp140.dll is the component the requirement is stated against and the one
// onnxruntime.dll links, and it is versioned with the redistributable itself
// (14.44.x for the 2015-2022 redist as of this writing). It is read from
// System32 by absolute path rather than by name so the answer is the machine's
// runtime, not a private copy sitting next to some executable on PATH.
func vcRuntimeVersion() (major, minor uint16, ok bool) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	path := filepath.Join(root, "System32", "msvcp140.dll")
	if _, err := os.Stat(path); err != nil {
		return 0, 0, false
	}

	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return 0, 0, false
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return 0, 0, false
	}

	var fixed *windows.VS_FIXEDFILEINFO
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &n); err != nil {
		return 0, 0, false
	}
	if fixed == nil || n < uint32(unsafe.Sizeof(*fixed)) {
		return 0, 0, false
	}
	// FileVersionMS packs major in the high word and minor in the low word.
	return uint16(fixed.FileVersionMS >> 16), uint16(fixed.FileVersionMS & 0xffff), true
}
