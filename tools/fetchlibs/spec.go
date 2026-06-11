package main

import "fmt"

// Pinned native-library versions. This tool is the single source of truth for
// these; the Makefile delegates fetching here, so versions live in one place.
const (
	ortVersion        = "1.24.3"
	graphqliteVersion = "0.6.0"
	tokenizersVersion = "v1.27.0"
)

// extractKind says how a downloaded artifact yields its target file.
type extractKind int

const (
	extractRaw   extractKind = iota // body is the file; write it straight to dest
	extractTarGz                    // gzip+tar; copy one member to dest
	extractZip                      // zip; copy one member to dest
)

// libSpec is a fully-resolved fetch plan for one native library on one platform.
type libSpec struct {
	id       string      // "ort" | "graphqlite" | "tokenizers"
	desc     string      // human label for logs
	url      string      // download URL
	extract  extractKind // how to turn the download into dest
	member   string      // path within the archive (extractTarGz/extractZip only)
	dest     string      // filename written into the destination dir
	codesign bool        // ad-hoc code-sign after writing (darwin hosts only)
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

// graphqliteSpec resolves the graphqlite fetch plan. The release asset is the
// bare shared library (no archive). macOS hosts ad-hoc code-sign it.
func graphqliteSpec(goos, goarch string) (libSpec, error) {
	var asset, dest string
	codesign := false
	switch goos {
	case "darwin":
		if goarch == "arm64" {
			asset = "graphqlite-macos-arm64.dylib"
		} else {
			asset = "graphqlite-macos-x86_64.dylib"
		}
		dest, codesign = "graphqlite.dylib", true
	case "linux":
		if goarch == "arm64" {
			asset = "graphqlite-linux-aarch64.so"
		} else {
			asset = "graphqlite-linux-x86_64.so"
		}
		dest = "graphqlite.so"
	case "windows":
		asset, dest = "graphqlite-windows-x86_64.dll", "graphqlite.dll"
	default:
		return libSpec{}, fmt.Errorf("graphqlite: unsupported platform %s/%s", goos, goarch)
	}

	return libSpec{
		id:       "graphqlite",
		desc:     "graphqlite v" + graphqliteVersion,
		url:      fmt.Sprintf("https://github.com/colliery-io/graphqlite/releases/download/v%s/%s", graphqliteVersion, asset),
		extract:  extractRaw,
		dest:     dest,
		codesign: codesign,
	}, nil
}

// tokenizersSpec resolves the daulet/tokenizers static-lib fetch plan. The
// project links libtokenizers.a statically (see internal/embeddings/cgo_link.go),
// so every build needs it. Upstream publishes darwin/linux only — there is NO
// Windows artifact through v1.27.0 — so Windows fails with an actionable error.
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
		return libSpec{}, fmt.Errorf("tokenizers: daulet/tokenizers ships no windows artifact at %s "+
			"(darwin/linux only); a windows build needs an upstream libtokenizers.a "+
			"or a from-source Rust build — see https://github.com/daulet/tokenizers/releases", tokenizersVersion)
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
	{"graphqlite", graphqliteSpec},
	{"tokenizers", tokenizersSpec},
}
