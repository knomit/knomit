package main

import (
	"strings"
	"testing"
)

func TestOrtSpec(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantURL      string
		wantMember   string
		wantDest     string
		wantExtract  extractKind
	}{
		{"darwin", "arm64",
			"https://github.com/microsoft/onnxruntime/releases/download/v1.24.3/onnxruntime-osx-arm64-1.24.3.tgz",
			"onnxruntime-osx-arm64-1.24.3/lib/libonnxruntime.1.24.3.dylib",
			"libonnxruntime.dylib", extractTarGz},
		{"darwin", "amd64",
			"https://github.com/microsoft/onnxruntime/releases/download/v1.24.3/onnxruntime-osx-x86_64-1.24.3.tgz",
			"onnxruntime-osx-x86_64-1.24.3/lib/libonnxruntime.1.24.3.dylib",
			"libonnxruntime.dylib", extractTarGz},
		{"linux", "amd64",
			"https://github.com/microsoft/onnxruntime/releases/download/v1.24.3/onnxruntime-linux-x64-1.24.3.tgz",
			"onnxruntime-linux-x64-1.24.3/lib/libonnxruntime.so.1.24.3",
			"libonnxruntime.so", extractTarGz},
		{"linux", "arm64",
			"https://github.com/microsoft/onnxruntime/releases/download/v1.24.3/onnxruntime-linux-aarch64-1.24.3.tgz",
			"onnxruntime-linux-aarch64-1.24.3/lib/libonnxruntime.so.1.24.3",
			"libonnxruntime.so", extractTarGz},
		{"windows", "amd64",
			"https://github.com/microsoft/onnxruntime/releases/download/v1.24.3/onnxruntime-win-x64-1.24.3.zip",
			"onnxruntime-win-x64-1.24.3/lib/onnxruntime.dll",
			"onnxruntime.dll", extractZip},
	}
	for _, c := range cases {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, err := ortSpec(c.goos, c.goarch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.url != c.wantURL {
				t.Errorf("url\n got: %s\nwant: %s", got.url, c.wantURL)
			}
			if got.member != c.wantMember {
				t.Errorf("member\n got: %s\nwant: %s", got.member, c.wantMember)
			}
			if got.dest != c.wantDest {
				t.Errorf("dest = %s, want %s", got.dest, c.wantDest)
			}
			if got.extract != c.wantExtract {
				t.Errorf("extract = %d, want %d", got.extract, c.wantExtract)
			}
		})
	}
}

func TestOrtSpecUnsupported(t *testing.T) {
	if _, err := ortSpec("plan9", "386"); err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
	}
}

func TestTokenizersSpec(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantURL      string
	}{
		{"darwin", "arm64",
			"https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.darwin-aarch64.tar.gz"},
		{"darwin", "amd64",
			"https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.darwin-x86_64.tar.gz"},
		{"linux", "amd64",
			"https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-amd64.tar.gz"},
		{"linux", "arm64",
			"https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-arm64.tar.gz"},
	}
	for _, c := range cases {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, err := tokenizersSpec(c.goos, c.goarch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.url != c.wantURL {
				t.Errorf("url\n got: %s\nwant: %s", got.url, c.wantURL)
			}
			if got.member != "libtokenizers.a" || got.dest != "libtokenizers.a" {
				t.Errorf("member/dest = %s/%s, want libtokenizers.a/libtokenizers.a", got.member, got.dest)
			}
			if got.extract != extractTarGz {
				t.Errorf("extract = %d, want extractTarGz", got.extract)
			}
		})
	}
}

// daulet/tokenizers publishes no Windows artifact (darwin/linux only through
// v1.27.0). Rather than fetching a 404 or refusing outright — which aborted
// `make build` and `make test` on Windows in their very first prerequisite,
// before anything else could run — the spec builds the crate from source.
func TestTokenizersSpecWindowsBuildsFromSource(t *testing.T) {
	spec, err := tokenizersSpec("windows", "amd64")
	if err != nil {
		t.Fatalf("windows tokenizers must resolve to a source build: %v", err)
	}
	if spec.extract != extractCargo {
		t.Errorf("extract = %v, want extractCargo", spec.extract)
	}
	if spec.dest != "libtokenizers.a" {
		t.Errorf("dest = %q, want libtokenizers.a — the name cgo_link_windows_amd64.go passes to -ltokenizers", spec.dest)
	}
	if spec.ref != tokenizersVersion {
		t.Errorf("ref = %q, want the pinned %q; a source build that drifts from the pin is not the same library", spec.ref, tokenizersVersion)
	}
	if spec.crate != tokenizersCrate {
		t.Errorf("crate = %q, want %q", spec.crate, tokenizersCrate)
	}
	// cargo normalises the dash in the package name to an underscore, and
	// getting this wrong fails only AFTER a multi-minute compile. The triple
	// is in the path because the build names a --target; see
	// TestTokenizersSpecPinsTheGnuTarget for why it must.
	if want := "target/" + tokenizersTarget + "/release/libtokenizers_ffi.a"; spec.member != want {
		t.Errorf("member = %q, want %q", spec.member, want)
	}
	if !strings.Contains(spec.url, "daulet/tokenizers") {
		t.Errorf("url = %q, want the daulet/tokenizers repo", spec.url)
	}
}

// Only amd64 is claimed. windows/arm64 has no verified toolchain story here,
// and silently attempting a build that cannot link is worse than saying so.
func TestTokenizersSpecWindowsArm64Unsupported(t *testing.T) {
	_, err := tokenizersSpec("windows", "arm64")
	if err == nil {
		t.Fatal("expected an error for windows/arm64")
	}
	if !strings.Contains(err.Error(), "arm64") {
		t.Errorf("error should name the platform: %v", err)
	}
}

// The target triple is load-bearing, not cosmetic. rustup's default host on
// Windows is x86_64-pc-windows-MSVC, whose output is an MSVC .lib that the
// MSYS2 mingw ld cannot link — cargo succeeds and the knomit link then fails
// with undefined references. Pinning -gnu makes the artifact depend on this
// constant rather than on the developer's toolchain default.
func TestTokenizersSpecPinsTheGnuTarget(t *testing.T) {
	spec, err := tokenizersSpec("windows", "amd64")
	if err != nil {
		t.Fatalf("tokenizersSpec: %v", err)
	}
	if spec.target != "x86_64-pc-windows-gnu" {
		t.Errorf("target = %q, want x86_64-pc-windows-gnu — an MSVC build cannot link with mingw", spec.target)
	}
	// --target changes where cargo puts the output; the two must agree or the
	// copy fails only AFTER a multi-minute compile.
	want := "target/" + spec.target + "/release/libtokenizers_ffi.a"
	if spec.member != want {
		t.Errorf("member = %q, want %q — naming a --target moves the output under the triple", spec.member, want)
	}
}
