package fact

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// A skill (F08, user rulings R6/R8) is a folder of instructions in the repo:
// `.knomit/skills/<name>/SKILL.md` in the Agent Skills format — YAML
// frontmatter with `name` and `description`, then a Markdown body — plus any
// other files in the folder, which are the skill's bundled files. knomit reads
// skills from the tip of the repo's consensus branch (ruling D2) and serves
// them over MCP (prompts, and the knomit_skill tool); it copies them nowhere.
// This file is the format half: the paths, the frontmatter rule, the
// $ARGUMENTS convention.

// SkillsDir is the repo folder holding one sub-folder per skill.
const SkillsDir = PrivateRoot + "/skills"

// SkillFileName is the file that makes a folder under SkillsDir a skill.
const SkillFileName = "SKILL.md"

// SkillArgumentsPlaceholder is replaced by the caller's arguments, the
// Agent Skills convention, so a SKILL.md written for a harness that expands
// it works unchanged when knomit serves it.
const SkillArgumentsPlaceholder = "$ARGUMENTS"

// SkillPath is the repo path of a skill's SKILL.md.
func SkillPath(name string) string { return SkillsDir + "/" + name + "/" + SkillFileName }

// ValidSkillName is the rule a skill name follows: lowercase kebab-case, the
// same rule as a recipe name, so it names exactly one folder.
func ValidSkillName(name string) bool { return validKeyRe.MatchString(name) }

// Skill is a parsed SKILL.md.
type Skill struct {
	Name        string
	Description string
	// Body is the Markdown after the frontmatter, leading blank lines trimmed.
	Body string
}

// ErrSkillNoFrontmatter is a SKILL.md that does not open with a `---` line.
var ErrSkillNoFrontmatter = errors.New("SKILL.md has no YAML frontmatter (it must start with a --- line)")

// ParseSkill parses the SKILL.md found in folder dir. It is malformed when the
// frontmatter is missing or not YAML, when `name` is not kebab-case or differs
// from the folder name, or when `description` is empty. Any other frontmatter
// key is allowed and ignored (the Agent Skills format has optional ones).
func ParseSkill(dir string, data []byte) (Skill, error) {
	s := string(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return Skill{}, ErrSkillNoFrontmatter
	}
	rest := s[len("---\n"):]
	var front, body string
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		front, body = "", strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n")
	} else {
		i := strings.Index(rest, "\n---\n")
		switch {
		case i >= 0:
			front, body = rest[:i], rest[i+len("\n---\n"):]
		case strings.HasSuffix(rest, "\n---"):
			front, body = strings.TrimSuffix(rest, "\n---"), ""
		default:
			return Skill{}, errors.New("SKILL.md frontmatter is not closed by a --- line")
		}
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return Skill{}, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	name := strings.TrimSpace(fm.Name)
	switch {
	case name == "":
		return Skill{}, errors.New("SKILL.md frontmatter has no name")
	case !ValidSkillName(name):
		return Skill{}, fmt.Errorf("skill name %q is not lowercase kebab-case", name)
	case name != dir:
		return Skill{}, fmt.Errorf("skill name %q differs from its folder %q", name, dir)
	}
	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return Skill{}, fmt.Errorf("skill %q has no description", name)
	}
	return Skill{Name: name, Description: desc, Body: strings.TrimLeft(body, "\n")}, nil
}

// ApplySkillArguments renders body for a caller that passed args: every
// $ARGUMENTS is replaced by args; a body with no placeholder gets the
// arguments appended as a final "Arguments: …" line; with no arguments the
// placeholder is removed and nothing is appended.
func ApplySkillArguments(body, args string) string {
	args = strings.TrimSpace(args)
	if strings.Contains(body, SkillArgumentsPlaceholder) {
		return strings.ReplaceAll(body, SkillArgumentsPlaceholder, args)
	}
	if args == "" {
		return body
	}
	return strings.TrimRight(body, "\n") + "\n\nArguments: " + args + "\n"
}
