// Command fetchlibs downloads knomit's native libraries (ONNX Runtime,
// and daulet/tokenizers' libtokenizers.a) into a destination
// directory. It is the cross-platform replacement for the bash/uname/make
// fetch logic: pure Go + stdlib, so it runs natively on Windows as well as
// macOS and Linux with no shell, curl, tar, or make.
//
// Usage:
//
//	go run ./tools/fetchlibs [-only ort,tokenizers] [dest-dir]
//
// dest-dir defaults to the per-platform lib dir, dist/<goos>-<goarch>/lib (the
// Makefile passes it explicitly). Each library is skipped if its target file is
// already present, so the command is idempotent and safe to re-run.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	only := flag.String("only", "", "comma-separated subset to fetch (ort,tokenizers); default all")
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

	fmt.Printf("%s installed to %s\n", spec.dest, destPath)
	return nil
}

// fetchClient bounds connection setup and header wait so an unreachable or
// silent mirror fails the build instead of hanging it. No Client.Timeout: the
// ONNX Runtime archives are large and a slow-but-live link must still finish.
var fetchClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	},
}

// httpGet performs a GET and returns the body, erroring on non-2xx so a 404
// release URL fails loudly instead of writing an HTML error page to disk.
func httpGet(url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// downloadTo streams a URL to a destination path, atomically.
func downloadTo(url, destPath string) error {
	body, err := httpGet(url)
	if err != nil {
		return err
	}
	defer body.Close()
	return writeFile(destPath, body)
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

// writeFile copies r into dst atomically: it writes a temp file alongside dst
// and renames it into place only after the copy succeeds.
//
// This matters because fetch() is skip-if-present. Writing dst directly meant an
// interrupted download (Ctrl-C, dropped connection, full disk) left a truncated
// library at the final path, and every later run said "already present,
// skipping" — poisoning the cache permanently, with the damage surfacing much
// later as a link or dlopen failure.
func writeFile(dst string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Same directory as dst: os.Rename is only atomic within one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".part-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed away
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Libraries are dlopen'd/linked, so they need the executable bit that
	// CreateTemp's 0600 does not give them.
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}
