// Command fetchlibs downloads knomit's native libraries (ONNX Runtime,
// graphqlite, and daulet/tokenizers' libtokenizers.a) into a destination
// directory. It is the cross-platform replacement for the bash/uname/make
// fetch logic: pure Go + stdlib, so it runs natively on Windows as well as
// macOS and Linux with no shell, curl, tar, or make.
//
// Usage:
//
//	go run ./tools/fetchlibs [-only ort,graphqlite,tokenizers] [dest-dir]
//
// dest-dir defaults to the per-platform lib dir, dist/<goos>-<goarch>/lib (the
// Makefile passes it explicitly). Each library is skipped if its target file is
// already present, so the command is idempotent and safe to re-run.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	only := flag.String("only", "", "comma-separated subset to fetch (ort,graphqlite,tokenizers); default all")
	flag.Parse()

	destDir := filepath.Join("dist", runtime.GOOS+"-"+runtime.GOARCH, "lib")
	if flag.NArg() > 0 {
		destDir = flag.Arg(0)
	}

	if err := run(destDir, *only, runtime.GOOS, runtime.GOARCH); err != nil {
		fmt.Fprintln(os.Stderr, "fetchlibs:", err)
		os.Exit(1)
	}
}

func run(destDir, only, goos, goarch string) error {
	wanted := map[string]bool{}
	for id := range strings.SplitSeq(only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	for _, b := range specBuilders {
		if len(wanted) > 0 && !wanted[b.id] {
			continue
		}
		spec, err := b.build(goos, goarch)
		if err != nil {
			return err
		}
		if err := fetch(spec, destDir, goos); err != nil {
			return fmt.Errorf("%s: %w", spec.id, err)
		}
	}
	return nil
}

// fetch downloads and installs one library, skipping work if dest already exists.
func fetch(spec libSpec, destDir, goos string) error {
	destPath := filepath.Join(destDir, spec.dest)
	if _, err := os.Stat(destPath); err == nil {
		fmt.Printf("%s already present at %s, skipping.\n", spec.dest, destPath)
		return nil
	}

	fmt.Printf("Downloading %s...\n", spec.desc)
	switch spec.extract {
	case extractRaw:
		if err := downloadTo(spec.url, destPath); err != nil {
			return err
		}
	case extractTarGz:
		resp, err := httpGet(spec.url)
		if err != nil {
			return err
		}
		defer resp.Close()
		if err := extractTarGzMember(destPath, spec.member, resp); err != nil {
			return err
		}
	case extractZip:
		tmp, err := os.CreateTemp("", "fetchlibs-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if err := downloadToFile(spec.url, tmp); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		if err := extractZipMember(destPath, spec.member, tmp.Name()); err != nil {
			return err
		}
	}

	// graphqlite ships unsigned; macOS Gatekeeper refuses to dlopen an unsigned
	// dylib, so ad-hoc sign it. Only meaningful on a darwin host.
	if spec.codesign && goos == "darwin" {
		cmd := exec.Command("codesign", "--sign", "-", "--force", destPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("codesign %s: %w: %s", destPath, err, out)
		}
	}

	fmt.Printf("%s installed to %s\n", spec.dest, destPath)
	return nil
}

// httpGet performs a GET and returns the body, erroring on non-2xx so a 404
// release URL fails loudly instead of writing an HTML error page to disk.
func httpGet(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url) //nolint:gosec // URL is a pinned release asset
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// downloadTo streams a URL straight to a destination path.
func downloadTo(url, destPath string) error {
	body, err := httpGet(url)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, body); err != nil {
		return err
	}
	return f.Close()
}

// downloadToFile streams a URL into an already-open file.
func downloadToFile(url string, f *os.File) error {
	body, err := httpGet(url)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = io.Copy(f, body)
	return err
}

// memberMatch reports whether a tar/zip entry name refers to the target member,
// tolerating a leading "./" that some archives prepend.
func memberMatch(entry, member string) bool {
	return path.Clean(strings.TrimPrefix(entry, "./")) == member
}

// extractTarGzMember reads a gzip+tar stream and writes the named member to dst.
func extractTarGzMember(dst, member string, src io.Reader) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("member %q not found in archive", member)
		}
		if err != nil {
			return err
		}
		if memberMatch(h.Name, member) {
			return writeFile(dst, tr)
		}
	}
}

// extractZipMember opens a zip file at zipPath and writes the named member to dst.
func extractZipMember(dst, member, zipPath string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if memberMatch(f.Name, member) {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(dst, rc)
		}
	}
	return fmt.Errorf("member %q not found in archive", member)
}

// writeFile copies r into a freshly created file at dst.
func writeFile(dst string, r io.Reader) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
