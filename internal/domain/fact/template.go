package fact

import (
	"fmt"
	"strings"
)

// F24: a template is a folder of files a new repo is created from, and a fact
// that describes it.
//
//   - Its FILES are everything under TemplatesDir/<name>/ in a mounted repo,
//     read at that repo's consensus tip. Exactly two kinds of path are
//     allowed in it (TemplatePathAllowed): the root README.md and anything
//     under .knomit/. They land at the same relative path in the new repo.
//   - Its DESCRIPTION is the `part: template` fact directly under
//     <ontology root>/templates/<name>/ in the same repo, at the same commit
//     (the knomit-playbooks layout). The listing shows that fact's title.
//
// A template brings no facts: nothing under the ontology root is ever copied.

// TemplatesDir is the repo folder holding one sub-folder per template.
const TemplatesDir = PrivateRoot + "/templates"

// TemplateReadme is the one root-level file a template may carry.
const TemplateReadme = "README.md"

// Template limits: a template is refused, never truncated, past either one.
const (
	TemplateMaxFiles = 256
	TemplateMaxBytes = 4 << 20 // 4 MiB, all files together
)

// Context keys and values of a template's describing fact (the playbooks
// ontology declares them on its `templates` topic).
const (
	TemplateContextName = "template"
	TemplateContextPart = "part"
	TemplatePartValue   = "template"
)

// TemplateFactsDir is the folder, under an ontology root, whose
// <name>/<id>.md children describe templates.
const TemplateFactsDir = "templates"

// ValidTemplateName is the rule a template name follows wherever it comes
// from (the request, a folder, a fact's context): the recipe-name rule,
// lowercase kebab-case, so a name can never reach outside TemplatesDir.
func ValidTemplateName(name string) bool { return ValidRecipeName(name) }

// TemplateFolder is the repo path of template name's folder.
func TemplateFolder(name string) string { return TemplatesDir + "/" + name }

// TemplatePathAllowed reports whether rel, a path relative to a template's
// folder (and so the path it lands at in the new repo), may be copied: the
// root README.md exactly, or a file under .knomit/. Everything else is
// refused, never skipped: a kb/ file would be a seed fact (out of scope), a
// root LICENSE or .gitmodules is a file knomit never writes, and a
// readme.md beside README.md is a case-duplicate a forge resolves as the
// README.
func TemplatePathAllowed(rel string) bool {
	if rel == TemplateReadme {
		return true
	}
	rest, ok := strings.CutPrefix(rel, PrivateRoot+"/")
	return ok && rest != "" && !strings.HasSuffix(rest, "/")
}

// TemplateFact is the description of one template, read from its
// `part: template` fact.
type TemplateFact struct {
	Name  string // the folder name, equal to context.template
	Path  string // the fact's repo path
	Title string
}

// ParseTemplateFact reads a `part: template` fact found at path, the direct
// child of <root>/templates/<folder>/. It returns ok=false, with no error,
// for a fact that is not a template description (another part); an error
// for a description that is malformed or names a different template than
// its folder.
func ParseTemplateFact(path, folder, content string) (TemplateFact, bool, error) {
	f, err := ParseFact(path, content)
	if err != nil {
		return TemplateFact{}, false, err
	}
	if part, _ := f.Context[TemplateContextPart].(string); part != TemplatePartValue {
		return TemplateFact{}, false, nil
	}
	name, _ := f.Context[TemplateContextName].(string)
	if name != folder {
		return TemplateFact{}, false, fmt.Errorf("template fact %s: context %s %q does not match its folder %q", path, TemplateContextName, name, folder)
	}
	if !ValidTemplateName(name) {
		return TemplateFact{}, false, fmt.Errorf("template fact %s: invalid template name %q", path, name)
	}
	return TemplateFact{Name: name, Path: path, Title: f.Title}, true, nil
}
