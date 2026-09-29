package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Skills (F08 PR C, user rulings R6/R8/D2) are `.knomit/skills/<name>/SKILL.md`
// folders served over MCP two ways: as prompts (prompts.go; user-invoked slash
// commands in Claude Code) and through the knomit_skill tool (skill_tool.go;
// model-invocable). Both read through the one skillSource below, so both
// answer the same three questions the same way:
//
//   - WHICH REPO: the binding's WRITE repo. A repo-scoped mount is that repo,
//     a lens is its write repo only (a repo mounted read-only in a lens serves
//     no skills there), and the unscoped mount has no repo at all.
//   - WHICH COMMIT: the tip of the repo's consensus branch,
//     UpstreamTip(svc.UpstreamBranch()) — never the agent branch, never an
//     experiment, never a branch name spelled here (ruling D2, and the
//     addendum: "main" means the repo's configured consensus branch). A
//     session's own unmerged edit therefore changes nothing it is told, and a
//     subscription, which has no agent branch, serves skills too.
//   - WHAT IS MALFORMED: a SKILL.md that fact.ParseSkill refuses is skipped
//     from every list and WARNed once per blob, never fatal to the list.

// skillInlineCap is the TOTAL size of bundled text files inlined into one
// skill's answer. Past it, files are listed by path, not inlined.
const skillInlineCap = 256 * 1024

// errSkillsUnscoped is what the unscoped mount answers for prompts: it has no
// repo, and prompts carry no binding handle to name one.
var errSkillsUnscoped = errors.New("bind a repo: prompts are served on repo-scoped mounts " +
	"(a bridge started with --repo or --lens); on this endpoint call the knomit_skill tool with your binding handle")

// skillSource resolves, reads and caches skills for one MCP server.
type skillSource struct {
	mu sync.Mutex
	// parsed caches fact.ParseSkill by the SKILL.md blob hash.
	parsed map[string]parsedSkill
	// warned records the malformed blobs already logged.
	warned map[string]bool
}

type parsedSkill struct {
	skill fact.Skill
	err   error
}

func newSkillSource() *skillSource {
	return &skillSource{parsed: map[string]parsedSkill{}, warned: map[string]bool{}}
}

// skillRepo is the repo whose skills a request is served: the binding's write
// repo, or errSkillsUnscoped / the binding error when there is none.
func skillRepo(ctx context.Context) (*repos.RepoInstance, error) {
	if repos.SessionScoped(ctx) {
		// Prompts reach here with no handle. The knomit_skill tool never
		// does: its binding gate resolves the handle first and puts the
		// Binding in the context, so it goes through RequireBinding below.
		if _, ok := repos.BindingFromContextOpt(ctx); !ok {
			return nil, errSkillsUnscoped
		}
	}
	b, err := repos.RequireBinding(ctx)
	if err != nil {
		return nil, err
	}
	return b.Write(), nil
}

// skillSet is one repo's valid skills at the tip of its consensus branch.
type skillSet struct {
	repo   string
	branch string
	tip    plumbing.Hash
	skills []fact.Skill
	// malformed maps a folder name to why its SKILL.md was refused.
	malformed map[string]error
}

func (s *skillSet) find(name string) (fact.Skill, bool) {
	for _, sk := range s.skills {
		if sk.Name == name {
			return sk, true
		}
	}
	return fact.Skill{}, false
}

// load reads ri's skills at the tip of its consensus branch. A repo whose
// consensus branch does not exist yet has none.
func (src *skillSource) load(ctx context.Context, ri *repos.RepoInstance) (*skillSet, error) {
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, errStoreUnavailable
	}
	defer release()
	return src.loadFrom(ctx, ri.Name(), svc)
}

func (src *skillSource) loadFrom(ctx context.Context, repo string, svc *store.Service) (*skillSet, error) {
	branch := svc.UpstreamBranch()
	tip, err := svc.Triggers().UpstreamTip(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("skills: read %s of repo %s: %w", branch, repo, err)
	}
	set := &skillSet{repo: repo, branch: branch, tip: tip, malformed: map[string]error{}}
	if tip.IsZero() {
		return set, nil
	}
	entries, err := svc.Skills().SkillsAt(ctx, tip)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		sk, perr := src.parse(repo, e)
		if perr != nil {
			set.malformed[e.Dir] = perr
			continue
		}
		set.skills = append(set.skills, sk)
	}
	return set, nil
}

// parse is fact.ParseSkill cached by the SKILL.md blob, with the one WARN
// per malformed blob. The folder name joins the key because it is part of the
// check (name must equal folder): the same bytes in another folder can parse
// differently.
func (src *skillSource) parse(repo string, e store.SkillEntry) (fact.Skill, error) {
	key := e.Dir + "\x00" + e.Blob
	src.mu.Lock()
	p, ok := src.parsed[key]
	src.mu.Unlock()
	if !ok {
		sk, err := fact.ParseSkill(e.Dir, e.Data)
		p = parsedSkill{skill: sk, err: err}
		src.mu.Lock()
		src.parsed[key] = p
		src.mu.Unlock()
	}
	if p.err != nil {
		src.mu.Lock()
		first := !src.warned[key]
		src.warned[key] = true
		src.mu.Unlock()
		if first {
			log.Warn().Err(p.err).Str("repo", repo).Str("skill", e.Dir).Str("blob", e.Blob).
				Msg("malformed skill skipped")
		}
	}
	return p.skill, p.err
}

// skillFile is one bundled file as served: inlined text, or listed only.
type skillFile struct {
	Path string `json:"path"`
	URI  string `json:"uri"`
	// MIMEType and Text are set only when the file is inlined.
	MIMEType string `json:"mime_type,omitempty"`
	Text     string `json:"text,omitempty"`
	Size     int64  `json:"size"`
	// Reason is set only when the file is NOT inlined: "binary" or "over-cap".
	Reason string `json:"reason,omitempty"`
}

// Reasons a bundled file is listed rather than inlined.
const (
	skillFileBinary  = "binary"
	skillFileOverCap = "over-cap"
)

// skillFileURI names a bundled file for the reader:
// knomit://<repo>/.knomit/skills/<name>/<file>.
func skillFileURI(repo, name, file string) string {
	return "knomit://" + repo + "/" + fact.SkillsDir + "/" + name + "/" + file
}

// bundledFiles reads skill name's bundled files at set's tip: text files are
// inlined in path order while the running TOTAL stays within skillInlineCap;
// binaries and anything past the cap are listed by path.
func (src *skillSource) bundledFiles(ctx context.Context, ri *repos.RepoInstance, set *skillSet, name string) (inlined, listed []skillFile, err error) {
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, nil, errStoreUnavailable
	}
	defer release()
	files, err := svc.Skills().SkillFilesAt(ctx, set.tip, name)
	if err != nil {
		return nil, nil, err
	}
	var used int64
	for _, f := range files {
		sf := skillFile{Path: f.Path, URI: skillFileURI(set.repo, name, f.Path), Size: f.Size}
		if used+f.Size > skillInlineCap {
			// Checked before reading: a large file is never loaded just to be
			// listed. Whether it is text does not matter — it would not fit.
			sf.Reason = skillFileOverCap
			listed = append(listed, sf)
			continue
		}
		data, rerr := f.Contents()
		if rerr != nil {
			return nil, nil, rerr
		}
		if isBinary(data) {
			sf.Reason = skillFileBinary
			listed = append(listed, sf)
			continue
		}
		used += f.Size
		sf.MIMEType = skillMIMEType(f.Path)
		sf.Text = string(data)
		inlined = append(inlined, sf)
	}
	return inlined, listed, nil
}

// isBinary is git's own heuristic (a NUL byte) plus invalid UTF-8, which an
// MCP text content cannot carry.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

// skillMIMEType is a fixed table, deliberately not mime.TypeByExtension: that
// reads the OS's tables (the Windows registry), so the same file would be
// served with a different type on different machines.
func skillMIMEType(p string) string {
	switch path.Ext(p) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".js", ".mjs":
		return "text/javascript"
	case ".py":
		return "text/x-python"
	case ".sh":
		return "text/x-shellscript"
	case ".html", ".htm":
		return "text/html"
	case ".csv":
		return "text/csv"
	default:
		return "text/plain"
	}
}

// errNoSkill is the named error for a skill that is not in the repo.
func errNoSkill(set *skillSet, name string) error {
	if reason, bad := set.malformed[name]; bad {
		return fmt.Errorf("skill %s in repo %s is malformed and is not served: %v", name, set.repo, reason)
	}
	if set.tip.IsZero() {
		return fmt.Errorf("no skill %s in repo %s: the repo has no %s branch yet, and skills are read from its tip", name, set.repo, set.branch)
	}
	return fmt.Errorf("no skill %s in repo %s (skills are read from the tip of %s; a skill only on an agent branch is not served until it reaches %s)",
		name, set.repo, set.branch, set.branch)
}
