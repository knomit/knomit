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
	"os/exec"
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

	if spec.extract == extractCargo {
		fmt.Printf("Building %s...\n", spec.desc)
	} else {
		fmt.Printf("Downloading %s...\n", spec.desc)
	}
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
	case extractCargo:
		if err := buildWithCargo(spec, destPath); err != nil {
			return err
		}
	}

	fmt.Printf("%s installed to %s\n", spec.dest, destPath)
	return nil
}

// TokenizersSrcEnv names a daulet/tokenizers checkout to build instead of
// cloning a fresh one. Set it to reuse a working tree — a clone of the tag
// pulls the whole crates.io index and every dependency, which is minutes of
// work this skips.
const TokenizersSrcEnv = "KNOMIT_TOKENIZERS_SRC"

// buildWithCargo produces spec.dest by compiling the Rust crate, for the one
// platform upstream publishes no artifact for.
//
// This is the only part of fetchlibs that is not pure Go + stdlib, and it is
// gated behind a platform that has no alternative — see tokenizersSpec. The
// tool checks for cargo and git UP FRONT rather than letting exec fail midway,
// because "exec: cargo: executable file not found in %PATH%" halfway through a
// `make build` does not tell anyone to install Rust.
func buildWithCargo(spec libSpec, destPath string) error {
	for _, tool := range []string{"git", "cargo"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf(
				"%s is needed to build %s from source and is not on PATH.\n"+
					"  Rust (which provides cargo): https://rustup.rs\n"+
					"  Then add the target this build needs:\n"+
					"      rustup target add %s\n"+
					"  Already have a %s checkout? Point %s at it to skip the clone.",
				tool, spec.dest, spec.target, spec.url, TokenizersSrcEnv)
		}
	}

	src := os.Getenv(TokenizersSrcEnv)
	if src == "" {
		dir, err := os.MkdirTemp("", "knomit-tokenizers-*")
		if err != nil {
			return fmt.Errorf("create build dir: %w", err)
		}
		// Best effort: cargo leaves read-only files in target/, and Windows
		// refuses to remove those. A leftover temp dir is not worth failing a
		// build that otherwise succeeded.
		defer func() { _ = os.RemoveAll(dir) }()

		fmt.Printf("Cloning %s at %s...\n", spec.url, spec.ref)
		if err := runTool(dir, "git", "clone", "--depth", "1", "--branch", spec.ref, spec.url, "."); err != nil {
			return fmt.Errorf("clone %s at %s: %w", spec.url, spec.ref, err)
		}
		src = dir
	} else {
		fmt.Printf("Building from the checkout named by %s: %s\n", TokenizersSrcEnv, src)
	}

	fmt.Printf("Building %s for %s with cargo (this takes a few minutes the first time)...\n", spec.crate, spec.target)
	if err := runTool(src, "cargo", "build", "--release", "-p", spec.crate, "--target", spec.target); err != nil {
		return fmt.Errorf("cargo build -p %s --target %s: %w\n"+
			"  If cargo says the target is not installed: rustup target add %s",
			spec.crate, spec.target, err, spec.target)
	}

	built := filepath.Join(src, filepath.FromSlash(spec.member))
	f, err := os.Open(built)
	if err != nil {
		return fmt.Errorf("cargo reported success but %s is missing: %w", built, err)
	}
	defer f.Close()
	return writeFile(destPath, f)
}

// runTool runs a build command in dir with its output attached, so a clone or
// a compile that takes minutes shows progress rather than appearing hung.
func runTool(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
//
// KNOWN LIMITATION on Windows: the final rename cannot replace a file that a
// running process has mapped, so re-fetching onnxruntime.dll while a knomit
// build is running fails with a sharing violation rather than succeeding.
// Nothing here works around it, because skip-if-present means the only way to
// reach this line for an existing dll is to delete it first — at which point
// nothing has it open. Stop the running binary if a fetch ever does report it.
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
