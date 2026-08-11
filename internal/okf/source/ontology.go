package source

import (
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"knomit/internal/fact"
	"knomit/internal/okf"
)

const okfOntologyRoot = "kb"

// okfOntologyFile is the ontology committed in the repo tree; okfOntologyFileLegacy
// is where it lived before moving into knomit's private namespace. Reading it
// at the SOURCE COMMIT (rather than from the live repo instance) keeps the
// bundle a pure function of that commit — the same determinism guarantee the
// facts get.
//
// Aliased to fact.OntologyFile / fact.LegacyOntologyFile rather than
// redeclared: this package used to carry its own duplicated literals, which
// is exactly the drift those constants exist to prevent.
const (
	okfOntologyFile       = fact.OntologyFile
	okfOntologyFileLegacy = fact.LegacyOntologyFile
)

// okfOntologyDoc reads and flattens the authored ontology at sourceSHA. A
// missing or unparseable ontology is not an error: the bundle is still fully
// conformant without descriptions, so it degrades to an empty doc and a
// warning.
func okfOntologyDoc(st storer.EncodedObjectStorer, sourceSHA plumbing.Hash) (okf.OntologyDoc, []string) {
	var warnings []string
	// Mirror how the repo itself resolves its ontology (repos/builder.go):
	// a committed ontology (canonical, then legacy) wins, otherwise the
	// embedded default, which is what the repo is actually being validated
	// against. The default is compiled in, so it is stable for a given build.
	ont := fact.DefaultOntology()
	if commit, err := object.GetCommit(st, sourceSHA); err == nil {
		f, ferr := commit.File(okfOntologyFile)
		if ferr != nil {
			f, ferr = commit.File(okfOntologyFileLegacy)
		}
		if ferr == nil {
			if content, err := f.Contents(); err == nil {
				parsed, perr := fact.ParseOntology([]byte(content))
				if perr != nil {
					warnings = append(warnings, "ontology parse: "+perr.Error())
				} else {
					ont = parsed
				}
			}
		}
	}
	if ont == nil {
		return okf.OntologyDoc{}, warnings
	}
	doc := okf.OntologyDoc{
		Name:        strings.TrimSpace(ont.Name),
		Description: strings.TrimSpace(ont.Description),
		Nodes:       map[string]string{},
	}
	var walk func(prefix string, nodes map[string]*fact.OntologyNode)
	walk = func(prefix string, nodes map[string]*fact.OntologyNode) {
		for name, n := range nodes {
			if n == nil {
				continue
			}
			key := name
			if prefix != "" {
				key = prefix + "/" + name
			}
			if d := strings.TrimSpace(n.Description); d != "" {
				doc.Nodes[key] = d
			}
			walk(key, n.Children)
		}
	}
	walk("", ont.Topics)
	return doc, warnings
}
