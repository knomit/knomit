// Package playbooks locates the pinned knomit-playbooks checkout that tests
// read templates and artifacts from.
//
// The mission template and the claude-session recipe live in the
// knomit-playbooks repository (https://github.com/knomit/knomit-playbooks), not
// in this one. They are checked out as the git submodule
// third_party/knomit-playbooks, pinned to one commit, so a test always runs
// the exact files of that commit.
//
// A missing or empty checkout FAILS the test, with the command that fixes it.
// It never skips: a skipped mission test would read as a pass.
package playbooks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// SubmodulePath is the checkout's path from the repository root.
const SubmodulePath = "third_party/knomit-playbooks"

// MissionTemplatePath is the mission template's folder inside the checkout.
const MissionTemplatePath = ".knomit/templates/mission"

// ManifestFile is a template's manifest. It describes the template and is
// NOT part of the repo created from it.
const ManifestFile = "TEMPLATE.md"

// FixHint is what a failure tells the reader to run.
const FixHint = "run `git submodule update --init` at the repository root"

// sentinel is a file every usable checkout has: the mission template's ontology.
var sentinel = filepath.Join(MissionTemplatePath, ".knomit", "ontology.yaml")

// Dir returns the knomit-playbooks checkout, failing the test if it is
// missing or empty.
func Dir(t testing.TB) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("knomit-playbooks: %v", err)
	}
	return dirAt(t, root)
}

// dirAt is Dir for the repository rooted at root.
func dirAt(t testing.TB, root string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(SubmodulePath))
	if err := Check(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Check reports why dir is not a usable knomit-playbooks checkout, or nil.
func Check(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, sentinel)); err != nil {
		return fmt.Errorf("knomit-playbooks checkout at %s is missing or empty (no %s): %s",
			dir, filepath.ToSlash(sentinel), FixHint)
	}
	return nil
}

// MissionTemplate returns the mission template's folder in the checkout.
func MissionTemplate(t testing.TB) string {
	t.Helper()
	return filepath.Join(Dir(t), filepath.FromSlash(MissionTemplatePath))
}

// Path returns a file or folder of the checkout by its slash path.
func Path(t testing.TB, rel string) string {
	t.Helper()
	return filepath.Join(Dir(t), filepath.FromSlash(rel))
}

// moduleRoot walks up from the working directory (a package directory under
// `go test`) to the folder holding go.mod.
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		up := filepath.Dir(d)
		if up == d {
			return "", fmt.Errorf("no go.mod above %s", wd)
		}
		d = up
	}
}
