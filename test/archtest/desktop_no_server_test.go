package archtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// desktopForbidden are the packages that ARE the knomit server: the
// composition root (internal/app) and the HTTP surface (internal/web). The
// desktop runs `knomit serve --supervised` as a child process and reaches it
// over the local socket/pipe; linking either of these back into knomit-desktop
// would be a second server construction path, which is exactly what the
// separate-process design removed (the two paths drifted: kb gotcha
// desktop/separate-server-boot-path).
var desktopForbidden = []string{"knomit/internal/app", "knomit/internal/web"}

// desktopForbiddenExact are packages the desktop must not link, matched
// exactly: the cgo inference stack (the ONNX runtime binding, the tokenizers
// library, and internal/embeddings, which links both). The desktop only needs
// to KNOW about the model for its boot caption, and that lives in the cgo-free
// internal/domain/modelparams, which a prefix match would wrongly catch.
var desktopForbiddenExact = []string{
	"knomit/internal/embeddings",
	"github.com/yalue/onnxruntime_go",
	"github.com/daulet/tokenizers",
}

// TestDesktopLinksNoServer walks the TRANSITIVE import graph of knomit-desktop
// (go list -deps resolves imports without compiling, so no cgo or native libs
// are needed) on each OS the desktop ships for, and fails if a server package
// appears. Transitive on purpose: a helper package that imports internal/app
// would put the whole server back in the binary as surely as a direct import.
// Test files are not part of -deps, so a desktop test may still use
// internal/app as a reference (the logging-options parity test does).
func TestDesktopLinksNoServer(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		cmd := exec.Command("go", "list", "-deps", "-tags", "desktop", "knomit/tools/desktop")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: go list -deps failed: %v\n%s", goos, err, out)
		}
		deps := strings.Fields(string(out))
		// Positive control: the graph really is the desktop's (it links Wails
		// and its own main package), so an empty or wrong listing cannot pass.
		if !contains(deps, "knomit/tools/desktop") || !contains(deps, "github.com/wailsapp/wails/v3/pkg/application") {
			t.Fatalf("%s: go list did not return the desktop's import graph:\n%s", goos, out)
		}
		for _, f := range desktopForbiddenExact {
			if contains(deps, f) {
				t.Errorf("%s: knomit-desktop links %s; the desktop must not link the inference stack (the boot caption uses internal/domain/modelparams)", goos, f)
			}
		}
		for _, dep := range deps {
			for _, f := range desktopForbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					t.Errorf("%s: knomit-desktop transitively imports %s; the desktop must not link the server (it supervises `knomit serve`)", goos, dep)
				}
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
