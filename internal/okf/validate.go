package okf

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Validate checks a bundle against OKF v0.2's conformance rules — the version
// bundles declare in their root index.md. The three rules are unchanged from
// v0.1; v0.2 only adds optional frontmatter (sources, generated, status), so
// nothing here needed to move. It returns the first violation found
// (deterministic: files are checked in sorted order as Build emits them).
func Validate(b Bundle) error {
	for _, f := range b.Files {
		if !strings.HasSuffix(f.Path, ".md") {
			continue
		}
		base := path.Base(f.Path)
		isReserved := base == "index.md" || base == "log.md"

		fm, _, ok := splitFrontmatter(f.Content)
		if isReserved {
			// Reserved files: if frontmatter is present it must parse; only the
			// root index may carry okf_version. No type requirement.
			if ok {
				var m map[string]any
				if err := yaml.Unmarshal(fm, &m); err != nil {
					return fmt.Errorf("okf: %s: unparseable frontmatter: %w", f.Path, err)
				}
			}
			continue
		}
		// Non-reserved concept docs: frontmatter required, type non-empty.
		if !ok {
			return fmt.Errorf("okf: %s: missing YAML frontmatter", f.Path)
		}
		var m map[string]any
		if err := yaml.Unmarshal(fm, &m); err != nil {
			return fmt.Errorf("okf: %s: unparseable frontmatter: %w", f.Path, err)
		}
		typ, _ := m["type"].(string)
		if strings.TrimSpace(typ) == "" {
			return fmt.Errorf("okf: %s: empty or missing type", f.Path)
		}
	}
	return nil
}

// splitFrontmatter returns the YAML frontmatter block (between leading "---"
// fences) and the remaining body. ok is false when no fenced block is present.
func splitFrontmatter(content []byte) (fm, body []byte, ok bool) {
	if !bytes.HasPrefix(content, []byte("---\n")) {
		return nil, content, false
	}
	rest := content[len("---\n"):]
	i := bytes.Index(rest, []byte("\n---\n"))
	if i < 0 {
		return nil, content, false
	}
	return rest[:i+1], rest[i+len("\n---\n"):], true
}
