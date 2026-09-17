package main

import "fmt"

// Pinned native-library versions. This tool is the single source of truth for
// these; the Makefile delegates fetching here, so versions live in one place.
const (
	ortVersion        = "1.24.3"
	tokenizersVersion = "v1.27.0"

	// Source-build coordinates, used only where no artifact is published
	// (windows). tokenizersCrate is the cargo package inside the workspace;
	// the repo root builds the wasm crate too, which needs a toolchain nobody
	// installing knomit should have to have.
	tokenizersRepo  = "https://github.com/daulet/tokenizers"
	tokenizersCrate = "tokenizers-ffi"

	// tokenizersTarget pins the Rust target triple, and pinning it is not
	// optional. rustup's DEFAULT host on Windows is x86_64-pc-windows-MSVC,
	// which produces an MSVC-format .lib; knomit is built with the MSYS2
	// mingw-w64 gcc, and mingw's ld cannot link that. The result is a
	// successful cargo build followed by an incomprehensible wall of
	// undefined references.
	//
	// Naming -gnu explicitly makes the output depend on the pin rather than on
	// how the developer's toolchain happens to be configured. It needs
	// `rustup target add x86_64-pc-windows-gnu`, which buildWithCargo says.
	tokenizersTarget = "x86_64-pc-windows-gnu"
)

// extractKind says how a downloaded artifact yields its target file.
type extractKind int

const (
	extractRaw   extractKind = iota // body is the file; write it straight to dest
	extractTarGz                    // gzip+tar; copy one member to dest
	extractZip                      // zip; copy one member to dest
	extractCargo                    // no download: build the Rust crate from source
)

// libSpec is a fully-resolved fetch plan for one native library on one platform.
type libSpec struct {
	id      string      // "ort" | "tokenizers"
	desc    string      // human label for logs
	url     string      // download URL, or the git repo for extractCargo
	extract extractKind // how to turn the download into dest
	member  string      // path within the archive, or within the checkout for extractCargo
	dest    string      // filename written into the destination dir

	// extractCargo only.
	ref    string // git tag or commit to check out
	crate  string // cargo package to build (-p)
	target string // Rust target triple (--target)
}

// ortSpec resolves the ONNX Runtime fetch plan. Releases ship as .tgz on
// macOS/Linux but .zip on Windows, with the lib under <root>/lib/.
func ortSpec(goos, goarch string) (libSpec, error) {
	var platform, ext string
	var member, dest string
	var kind extractKind
	switch goos {
	case "darwin":
		ext, kind = "tgz", extractTarGz
		if goarch == "arm64" {
			platform = "osx-arm64"
		} else {
			platform = "osx-x86_64"
		}
		dest = "libonnxruntime.dylib"
	case "linux":
		ext, kind = "tgz", extractTarGz
		if goarch == "arm64" {
			platform = "linux-aarch64"
		} else {
			platform = "linux-x64"
		}
		dest = "libonnxruntime.so"
	case "windows":
		ext, kind = "zip", extractZip
		platform = "win-x64"
		dest = "onnxruntime.dll"
	default:
		return libSpec{}, fmt.Errorf("onnxruntime: unsupported platform %s/%s", goos, goarch)
	}

	root := fmt.Sprintf("onnxruntime-%s-%s", platform, ortVersion)
	switch goos {
	case "darwin":
		member = fmt.Sprintf("%s/lib/libonnxruntime.%s.dylib", root, ortVersion)
	case "linux":
		member = fmt.Sprintf("%s/lib/libonnxruntime.so.%s", root, ortVersion)
	case "windows":
		member = fmt.Sprintf("%s/lib/onnxruntime.dll", root)
	}

	return libSpec{
		id:      "ort",
		desc:    "ONNX Runtime " + ortVersion,
		url:     fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/%s.%s", ortVersion, root, ext),
		extract: kind,
		member:  member,
		dest:    dest,
	}, nil
}

// tokenizersSpec resolves the daulet/tokenizers static-lib plan. The project
// links libtokenizers.a statically (see the per-platform
// internal/embeddings/cgo_link_*.go), so every build needs it.
//
// darwin and linux download a published artifact. Windows BUILDS FROM SOURCE:
// upstream publishes no windows asset (checked through v1.27.0), but the
// crate itself compiles there — v1.27.0 carries the windows cgo LDFLAGS added
// in daulet/tokenizers PR 61 — so the missing piece is the build, not the
// support. That is the one step this tool cannot do with the stdlib alone; it
// shells out to git and cargo, and says so plainly when either is absent.
//
// fetch() skips any library whose dest already exists, so a checkout that
// already has dist/windows-amd64/lib/libtokenizers.a never runs cargo.
func tokenizersSpec(goos, goarch string) (libSpec, error) {
	var plat string
	switch goos {
	case "darwin":
		if goarch == "arm64" {
			plat = "darwin-aarch64"
		} else {
			plat = "darwin-x86_64"
		}
	case "linux":
		if goarch == "arm64" {
			plat = "linux-arm64"
		} else {
			plat = "linux-amd64"
		}
	case "windows":
		if goarch != "amd64" {
			return libSpec{}, fmt.Errorf("tokenizers: unsupported platform %s/%s", goos, goarch)
		}
		return libSpec{
			id:      "tokenizers",
			desc:    "libtokenizers.a " + tokenizersVersion + " (built from source with cargo)",
			url:     tokenizersRepo,
			extract: extractCargo,
			ref:     tokenizersVersion,
			crate:   tokenizersCrate,
			target:  tokenizersTarget,
			// Where cargo leaves a staticlib. The crate is tokenizers-ffi and
			// cargo normalises the dash to an underscore; naming a --target
			// also inserts the triple into the output path.
			member: "target/" + tokenizersTarget + "/release/libtokenizers_ffi.a",
			dest:   "libtokenizers.a",
		}, nil
	default:
		return libSpec{}, fmt.Errorf("tokenizers: unsupported platform %s/%s", goos, goarch)
	}

	return libSpec{
		id:      "tokenizers",
		desc:    "libtokenizers.a " + tokenizersVersion,
		url:     fmt.Sprintf("https://github.com/daulet/tokenizers/releases/download/%s/libtokenizers.%s.tar.gz", tokenizersVersion, plat),
		extract: extractTarGz,
		member:  "libtokenizers.a",
		dest:    "libtokenizers.a",
	}, nil
}

// specBuilders maps a lib id to its resolver, in setup order.
var specBuilders = []struct {
	id    string
	build func(goos, goarch string) (libSpec, error)
}{
	{"ort", ortSpec},
	{"tokenizers", tokenizersSpec},
}
