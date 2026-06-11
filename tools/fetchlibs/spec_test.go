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
			if got.codesign {
				t.Errorf("ort should never be code-signed")
			}
		})
	}
}

func TestOrtSpecUnsupported(t *testing.T) {
	if _, err := ortSpec("plan9", "386"); err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
	}
}

func TestGraphqliteSpec(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantURL      string
		wantDest     string
		wantCodesign bool
	}{
		{"darwin", "arm64",
			"https://github.com/colliery-io/graphqlite/releases/download/v0.6.0/graphqlite-macos-arm64.dylib",
			"graphqlite.dylib", true},
		{"darwin", "amd64",
			"https://github.com/colliery-io/graphqlite/releases/download/v0.6.0/graphqlite-macos-x86_64.dylib",
			"graphqlite.dylib", true},
		{"linux", "amd64",
			"https://github.com/colliery-io/graphqlite/releases/download/v0.6.0/graphqlite-linux-x86_64.so",
			"graphqlite.so", false},
		{"linux", "arm64",
			"https://github.com/colliery-io/graphqlite/releases/download/v0.6.0/graphqlite-linux-aarch64.so",
			"graphqlite.so", false},
		{"windows", "amd64",
			"https://github.com/colliery-io/graphqlite/releases/download/v0.6.0/graphqlite-windows-x86_64.dll",
			"graphqlite.dll", false},
	}
	for _, c := range cases {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, err := graphqliteSpec(c.goos, c.goarch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.url != c.wantURL {
				t.Errorf("url\n got: %s\nwant: %s", got.url, c.wantURL)
			}
			if got.dest != c.wantDest {
				t.Errorf("dest = %s, want %s", got.dest, c.wantDest)
			}
			if got.extract != extractRaw {
				t.Errorf("graphqlite is a raw single-file download, got extract=%d", got.extract)
			}
			if got.codesign != c.wantCodesign {
				t.Errorf("codesign = %v, want %v", got.codesign, c.wantCodesign)
			}
		})
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
// v1.27.0), so the tool must fail with an actionable message rather than fetch
// a 404. This is the load-bearing edge case for "cross-platform".
func TestTokenizersSpecWindowsUnsupported(t *testing.T) {
	_, err := tokenizersSpec("windows", "amd64")
	if err == nil {
		t.Fatal("expected error for windows tokenizers, got nil")
	}
	if !strings.Contains(err.Error(), "windows") {
		t.Errorf("error should mention windows: %v", err)
	}
}
