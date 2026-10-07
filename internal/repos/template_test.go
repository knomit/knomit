package repos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/playbooks"
	"knomit/internal/testsupport/testsigner"
)

// ── fixtures ──────────────────────────────────────────────────────────────

const tmplAgent = "agent/test-abc"

// tmplManager is a started Manager whose filesystem origins may live under
// root, signing with a NAMED key: with the test fallback signer installed, a
// path that forgot SetSigner would still sign — just with the wrong key — so
// every signature assertion below compares against this key.
func tmplManager(t *testing.T, root, signer string) *Manager {
	t.Helper()
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: root},
		AgentBranch: tmplAgent,
		Signer:      testsigner.Named(signer),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

// templateFactBody is a playbooks-shaped `part: template` fact.
func templateFactBody(name, title string) string {
	return "---\ntype: reference\ndomain: [templates]\nconfidence: 0.9\nsources: 1\n" +
		"context: {part: template, template: " + name + "}\n---\n# " + title + "\n\nTemplate folder: `.knomit/templates/" + name + "/`.\n"
}

// sourceSpec is a hand-made template source: a git repo that is a knowledge
// base (its own ontology at the root) with whatever else a test puts in it.
type sourceSpec struct {
	files   map[string]string // repo path -> content
	links   map[string]string // repo path -> symlink target
	execs   []string          // repo paths to chmod +x (must be in files)
	branch  string            // default "main"
	noOntol bool
}

// sourceRepo builds sourceSpec as a bare repo under root and returns its URL.
func sourceRepo(t *testing.T, root, name string, s sourceSpec) string {
	t.Helper()
	if s.branch == "" {
		s.branch = "main"
	}
	work := t.TempDir()
	files := map[string]string{}
	for k, v := range s.files {
		files[k] = v
	}
	if !s.noOntol {
		ont, err := fact.DefaultOntology().Serialize()
		require.NoError(t, err)
		files[OntologyPath] = string(ont)
	}
	for p, c := range files {
		dst := filepath.Join(work, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		require.NoError(t, os.WriteFile(dst, []byte(c), 0o644))
	}
	for p, target := range s.links {
		dst := filepath.Join(work, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
		require.NoError(t, os.Symlink(target, dst))
	}
	runGit(t, work, "init", "-q", "-b", s.branch)
	runGit(t, work, "add", "-A")
	for _, p := range s.execs {
		runGit(t, work, "update-index", "--chmod=+x", p)
	}
	runGit(t, work, "commit", "-q", "-m", "source")
	bare := filepath.Join(root, name+".git")
	runGit(t, "", "init", "-q", "--bare", "-b", s.branch, bare)
	runGit(t, work, "push", "-q", bare, s.branch)
	return fileuri.New(bare)
}

// mount creates name from url in mode ("clone" or "subscribe").
func mount(t *testing.T, m *Manager, name, mode, url string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{Name: name, Mode: mode, Origin: &OriginSpec{URL: url}}, nil)
	require.NoError(t, err, "mount %s", name)
	return ri
}

// playbooksSource mounts the pinned knomit-playbooks checkout as "playbooks".
func playbooksSource(t *testing.T, m *Manager, root, mode string) *RepoInstance {
	t.Helper()
	bare := playbooks.SourceRepo(t, filepath.Join(root, "playbooks.git"), "main", nil)
	return mount(t, m, "playbooks", mode, fileuri.New(bare))
}

// templateDirFiles is the template folder as git sees it: path -> blob hash,
// read with real git from the checkout (independent of the code under test).
func templateDirBlobs(t *testing.T, name string) map[string]string {
	t.Helper()
	dir := playbooks.Dir(t)
	out, err := exec.Command("git", "-C", dir, "ls-tree", "-r", "HEAD", ".knomit/templates/"+name+"/").Output()
	require.NoError(t, err)
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		meta, path, _ := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		got[strings.TrimPrefix(path, ".knomit/templates/"+name+"/")] = f[2]
	}
	return got
}

// cloneBranch serves ri's store over smart HTTP and clones branch with real
// git, so the tree, the trailers and the signature are read by git itself.
func cloneBranch(t *testing.T, ri *RepoInstance, branch string) string {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	t.Cleanup(release)
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	runGit(t, "", "clone", "-q", "--branch", branch, srv.URL, dir)
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// treeBlobs is `git ls-tree -r rev` as path -> blob.
func treeBlobs(t *testing.T, dir, rev string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, line := range strings.Split(gitIn(t, dir, "ls-tree", "-r", rev), "\n") {
		meta, path, _ := strings.Cut(line, "\t")
		got[path] = strings.Fields(meta)[2]
	}
	return got
}

// noTraceOf asserts a failed create left nothing: no registry row for name,
// and no repo database file beyond those present before.
func noTraceOf(t *testing.T, m *Manager, name string, dbsBefore []string) {
	t.Helper()
	require.Nil(t, m.Get(name))
	for _, st := range []RepoState{StateActive, StateArchived} {
		recs, err := m.reg.List(st)
		require.NoError(t, err)
		for _, r := range recs {
			require.NotEqual(t, name, r.Name, "a refused create must leave no registry row")
		}
	}
	require.Equal(t, dbsBefore, repoFiles(t, m), "a refused create must leave no .db, -wal or -shm")
}

func repoFiles(t *testing.T, m *Manager) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(m.deps.Cfg.Home, "repos"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, filepath.Base(p))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ── Copy, signature, provenance ───────────────────────────────────────────

// F24 Verification "Copy" and "Provenance": the content commit's tree is the
// template folder exactly — the same blobs at the same paths, case kept
// (SKILL.md, README.md) — on top of the unsigned root commit; it is signed by
// THIS instance's key; it carries Knomit-Template and Knomit-Template-Source
// = kb://<source ID>@<source consensus tip>; the repo opens with the
// template's ontology; renaming the source afterwards changes nothing.
//
// SABOTAGE: lowercase WriteSystemTree's paths (blob map, SKILL.md); drop
// SetSigner in initTemplate (named key); write the source NAME in the
// trailer (ID assertion).
func TestCreateFromTemplate_CopiesTreeSignedWithTrailers(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	src := playbooksSource(t, m, root, "clone")
	srcID := src.ID()
	srcSvc, release, err := src.Acquire()
	require.NoError(t, err)
	srcTip, err := srcSvc.Triggers().UpstreamTip(context.Background(), srcSvc.UpstreamBranch())
	release()
	require.NoError(t, err)

	ri, err := m.Create(context.Background(), CreateSpec{Name: "my-mission", Mode: "template",
		Template: &TemplateRef{Repo: "playbooks", Name: "mission"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "mission", ri.Ontology().ID, "the repo opens with the template's ontology")

	dir := cloneBranch(t, ri, tmplAgent)
	content := gitIn(t, dir, "log", "-1", "--format=%H", "--grep=^Knomit-Template: mission$", tmplAgent)
	require.NotEmpty(t, content, "the template commit must carry Knomit-Template")
	require.Equal(t, templateDirBlobs(t, "mission"), treeBlobs(t, dir, content),
		"the content commit's tree must be the template folder, byte for byte, case kept")

	msg := gitIn(t, dir, "log", "-1", "--format=%B", content)
	require.Equal(t, "mission", store.TrailerValue(msg, store.TrailerTemplate))
	require.Equal(t, "kb://"+srcID+"@"+srcTip.String(), store.TrailerValue(msg, store.TrailerTemplateSource))
	require.NotContains(t, store.TrailerValue(msg, store.TrailerTemplateSource), "playbooks",
		"the source is recorded by ID, never by name")

	key, signed, err := testsigner.CommitSignerKey(dir, content)
	require.NoError(t, err)
	require.True(t, signed, "the template commit must be signed")
	require.Equal(t, testsigner.Named("tmpl-host").PublicKey().Marshal(), key.Marshal(),
		"signed by THIS instance's key, not the test fallback")

	parents := strings.Fields(gitIn(t, dir, "log", "-1", "--format=%P", content))
	require.Len(t, parents, 1)
	require.Equal(t, parents[0], gitIn(t, dir, "rev-list", "--max-parents=0", content), "one commit on top of the root")
	_, rootSigned, err := testsigner.CommitSignerKey(dir, parents[0])
	require.NoError(t, err)
	require.False(t, rootSigned, "the init root commit stays unsigned, by design")

	// Renaming the source afterwards changes nothing in the new repo.
	require.NoError(t, m.RenameRepo("playbooks", "renamed"))
	dir2 := cloneBranch(t, ri, tmplAgent)
	require.Equal(t, content, gitIn(t, dir2, "log", "-1", "--format=%H", "--grep=^Knomit-Template: mission$", tmplAgent))
}

// ── Source branch, subscription ───────────────────────────────────────────

// F24 Verification "Source branch": version A on the source's consensus
// branch, version B on its agent branch → the new repo gets A.
// SABOTAGE: read the source's ReadBranch/agent branch instead of
// UpstreamTip(UpstreamBranch()).
func TestCreateFromTemplate_ReadsConsensusNotAgent(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	url := sourceRepo(t, root, "src", sourceSpec{files: map[string]string{
		".knomit/templates/x/.knomit/ontology.yaml": "id: x-a\nname: A\ntopics:\n  notes:\n    description: A\n",
	}})
	src := mount(t, m, "src", "clone", url)
	svc, release, err := src.Acquire()
	require.NoError(t, err)
	_, _, err = svc.WriteSystemTree(context.Background(), src.AgentBranch(), map[string]string{
		".knomit/templates/x/.knomit/ontology.yaml": "id: x-b\nname: B\ntopics:\n  notes:\n    description: B\n",
	}, "B on the agent branch", "updated")
	release()
	require.NoError(t, err)

	ri, err := m.Create(context.Background(), CreateSpec{Name: "n", Mode: "template", Template: &TemplateRef{Repo: "src", Name: "x"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "x-a", ri.Ontology().ID, "the template is read at the source's consensus tip, never its agent branch")
}

// F24 Verification "Subscription" and "Subscriptions are read-only": a
// subscribed source has no agent branch and refuses writes, and a create
// from it gets the followed branch's version.
func TestCreateFromTemplate_FromSubscription(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	src := playbooksSource(t, m, root, "subscribe")
	require.Empty(t, src.AgentBranch(), "a subscription has no agent branch")
	require.False(t, src.WritableBranch(src.ReadBranch()), "a subscribed playbooks repo refuses every write")

	ri, err := m.Create(context.Background(), CreateSpec{Name: "kb", Mode: "template",
		Template: &TemplateRef{Repo: "playbooks", Name: "general"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "general", ri.Ontology().ID)
}

// ── Inert in the source ───────────────────────────────────────────────────

// F24 Verification "Inert in the source": the playbooks repo serves only its
// own skill (not the template's), indexes no file under .knomit/templates/,
// and resolves no template recipe as its own.
func TestTemplateSource_Inert(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	src := playbooksSource(t, m, root, "clone")
	svc, release, err := src.Acquire()
	require.NoError(t, err)
	defer release()
	ctx := context.Background()
	tip, err := svc.Triggers().UpstreamTip(ctx, svc.UpstreamBranch())
	require.NoError(t, err)
	skills, err := svc.Skills().SkillsAt(ctx, tip)
	require.NoError(t, err)
	var names []string
	for _, s := range skills {
		names = append(names, s.Dir)
	}
	require.Equal(t, []string{"program-knomit"}, names)
	it, err := svc.FactQuery().FactsIter(ctx, src.ReadBranch())
	require.NoError(t, err)
	n := 0
	for {
		row, err := it.Next()
		require.NoError(t, err)
		if row == nil {
			break
		}
		n++
		require.False(t, strings.HasPrefix(row.Path, ".knomit/"), "no template file is an indexed fact: %s", row.Path)
	}
	require.NoError(t, it.Close())
	require.Greater(t, n, 0, "the playbooks facts themselves are indexed")
	_, _, err = svc.Triggers().RecipeAt(ctx, tip, "work-task")
	require.Error(t, err, "the mission template's recipe is not the playbooks repo's recipe")
}

// ── Refusals ──────────────────────────────────────────────────────────────

// F24 Verification "Refusals": each refused with its named error, from
// CreatePreflight (the status before the 202) AND from Create (the
// authoritative path), and nothing left behind.
// SABOTAGE: remove any one check → its row goes red.
func TestCreateFromTemplate_Refusals(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	onto := "id: t\nname: T\ntopics:\n  notes:\n    description: N\n"
	url := sourceRepo(t, root, "src", sourceSpec{
		files: map[string]string{
			".knomit/templates/no-ontology/README.md":              "R",
			".knomit/templates/bad-ontology/.knomit/ontology.yaml": "id: [\n",
			".knomit/templates/not-new/.knomit/ontology.yaml":      notNewOntology(t),
			".knomit/templates/preset-id/.knomit/ontology.yaml":    "id: general\nname: G\ntopics:\n  mine:\n    description: not the preset\n",
			".knomit/templates/preset-attrs/.knomit/ontology.yaml": presetPlusRootAttr(t),
			".knomit/templates/kb-path/.knomit/ontology.yaml":      onto,
			".knomit/templates/kb-path/kb/notes/x.md":              "seed fact",
			".knomit/templates/license/.knomit/ontology.yaml":      onto,
			".knomit/templates/license/LICENSE":                    "L",
			".knomit/templates/symlink/.knomit/ontology.yaml":      onto,
			".knomit/templates/exec/.knomit/ontology.yaml":         onto,
			".knomit/templates/exec/.knomit/recipes/run.js":        "x",
			".knomit/templates/twice/.knomit/ontology.yaml":        onto,
			"kb/templates/twice/aaaa1111.md":                       templateFactBody("twice", "one"),
			"kb/templates/twice/bbbb2222.md":                       templateFactBody("twice", "two"),
			"kb/templates/fact-only/cccc3333.md":                   templateFactBody("fact-only", "no folder"),
			".knomit/templates/fleet/.knomit/ontology.yaml":        mustPresetYAML(t, "fleet"),
		},
		links: map[string]string{".knomit/templates/symlink/.knomit/link.md": "../README.md"},
		execs: []string{".knomit/templates/exec/.knomit/recipes/run.js"},
	})
	mount(t, m, "src", "clone", url)
	createRepo(t, m, "plain")
	_, err := m.CreateLens(context.Background(), Lens{Name: "a-lens", WriteUID: m.Get("plain").UID()})
	require.NoError(t, err)
	archived := mount(t, m, "gone", "clone", sourceRepo(t, root, "gone", sourceSpec{}))
	_ = archived
	_, err = m.Archive("gone")
	require.NoError(t, err)

	for _, tc := range []struct {
		ref  TemplateRef
		want error
	}{
		{TemplateRef{Repo: "nope", Name: "x"}, ErrTemplateSourceNotFound},
		{TemplateRef{Repo: "gone", Name: "x"}, ErrTemplateSourceNotFound},
		{TemplateRef{Repo: "a-lens", Name: "x"}, ErrTemplateSourceNotFound},
		{TemplateRef{Repo: "src", Name: "../x"}, ErrTemplateName},
		{TemplateRef{Repo: "src", Name: "absent"}, ErrTemplateNotFound},
		{TemplateRef{Repo: "src", Name: "fact-only"}, ErrTemplateNotFound},
		{TemplateRef{Repo: "src", Name: "no-ontology"}, ErrTemplateNoOntology},
		{TemplateRef{Repo: "src", Name: "bad-ontology"}, ErrTemplateOntology},
		{TemplateRef{Repo: "src", Name: "not-new"}, ErrTemplateOntology},
		{TemplateRef{Repo: "src", Name: "preset-id"}, ErrTemplatePresetID},
		{TemplateRef{Repo: "src", Name: "preset-attrs"}, ErrTemplatePresetID},
		{TemplateRef{Repo: "src", Name: "kb-path"}, ErrTemplateLayout},
		{TemplateRef{Repo: "src", Name: "license"}, ErrTemplateLayout},
		{TemplateRef{Repo: "src", Name: "symlink"}, ErrTemplateNotRegular},
		{TemplateRef{Repo: "src", Name: "exec"}, ErrTemplateNotRegular},
		{TemplateRef{Repo: "src", Name: "twice"}, ErrTemplateDescribedTwice},
		{TemplateRef{Repo: "src", Name: "fleet"}, ErrTemplateFleetLocal},
	} {
		t.Run(tc.ref.Repo+"/"+tc.ref.Name, func(t *testing.T) {
			before := repoFiles(t, m)
			spec := CreateSpec{Name: "new-repo", Mode: "template", Template: &tc.ref}
			require.True(t, errors.Is(m.CreatePreflight(context.Background(), spec), tc.want), "preflight: want %v", tc.want)
			_, err := m.Create(context.Background(), spec, nil)
			require.True(t, errors.Is(err, tc.want), "create: want %v, got %v", tc.want, err)
			noTraceOf(t, m, "new-repo", before)
		})
	}
}

// notNewOntology is an ontology the OPEN path accepts and the CREATE path
// refuses: a repository-level attribute declared on a topic is only a warning
// for an existing repo, and fatal for a new one. The fixture checks that it
// separates the two parsers, so the row that uses it pins ParseNewOntology
// (with ParseOntology in ResolveTemplate the template would be accepted).
func notNewOntology(t *testing.T) string {
	t.Helper()
	y := "id: t\nname: T\ntopics:\n  notes:\n    description: N\n    attributes:\n      verify_signatures: log\n"
	_, err := fact.ParseOntology([]byte(y))
	require.NoError(t, err, "fixture: the open path must accept it")
	_, err = fact.ParseNewOntology([]byte(y))
	require.Error(t, err, "fixture: the create path must refuse it")
	return y
}

// presetPlusRootAttr is the general preset, topics and all, plus a root
// attribute the preset does not declare: the boot refresh would erase it, so
// a template carrying the preset's id with it is not that preset.
func presetPlusRootAttr(t *testing.T) string {
	t.Helper()
	y := withRootAttr(t, mustPresetYAML(t, "default"), "enforce")
	parsed, err := fact.ParseNewOntology([]byte(y))
	require.NoError(t, err, "fixture: a valid new ontology")
	require.Equal(t, fact.DivergenceAttributes, refreshDivergence(parsed, fact.DefaultOntology()), "fixture: diverges only in its root attributes")
	return y
}

// withRootAttr appends a root `attributes: {verify_signatures: mode}` block
// to an ontology document that has none.
func withRootAttr(t *testing.T, y, mode string) string {
	t.Helper()
	require.NotContains(t, y, "\nattributes:", "fixture: no root attributes yet")
	return strings.TrimRight(y, "\n") + "\nattributes:\n  verify_signatures: " + mode + "\n"
}

func mustPresetYAML(t *testing.T, preset string) string {
	t.Helper()
	o, err := fact.OntologyByPreset(preset)
	require.NoError(t, err)
	y, err := o.Serialize()
	require.NoError(t, err)
	return string(y)
}

// F24: a template that IS a preset (the playbooks `general`, byte-identical
// to the embedded preset) keeps the preset's id and is accepted.
func TestCreateFromTemplate_PresetTemplateIsAccepted(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	playbooksSource(t, m, root, "clone")
	ri, err := m.Create(context.Background(), CreateSpec{Name: "kb", Mode: "template", Template: &TemplateRef{Repo: "playbooks", Name: "coding"}}, nil)
	require.NoError(t, err)
	require.Equal(t, "source-code", ri.Ontology().ID)
}

// N1 (#433 review, user ruling): "is that preset" is the boot refresh's own
// test, refreshDivergence, not SubsetDivergence. The two differ only when the
// PRESET declares a root attribute the template lacks (the refresh would ADD
// it), and no embedded preset declares one today, so a preset with one is
// stood in through templatePresetByID. The fixture asserts it separates the
// two functions, and the byte-identical template must be refused.
// SABOTAGE: call SubsetDivergence in ResolveTemplate → accepted → red.
func TestCreateFromTemplate_PresetIdentityIsTheRefreshRule(t *testing.T) {
	withRoot, err := fact.ParseOntology([]byte(withRootAttr(t, mustPresetYAML(t, "default"), "log")))
	require.NoError(t, err)
	orig := templatePresetByID
	templatePresetByID = func(id string) *fact.Ontology {
		if id == "general" {
			return withRoot
		}
		return orig(id)
	}
	t.Cleanup(func() { templatePresetByID = orig })

	plain := mustPresetYAML(t, "default")
	parsed, err := fact.ParseNewOntology([]byte(plain))
	require.NoError(t, err)
	require.Equal(t, "", parsed.SubsetDivergence(withRoot), "fixture: SubsetDivergence sees no divergence")
	require.Equal(t, fact.DivergenceAttributes, refreshDivergence(parsed, withRoot), "fixture: refreshDivergence does")

	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	mount(t, m, "src", "clone", sourceRepo(t, root, "src", sourceSpec{files: map[string]string{
		".knomit/templates/general/.knomit/ontology.yaml": plain,
	}}))
	_, err = m.Create(context.Background(), CreateSpec{Name: "kb", Mode: "template", Template: &TemplateRef{Repo: "src", Name: "general"}}, nil)
	require.ErrorIs(t, err, ErrTemplatePresetID)
	require.ErrorContains(t, err, fact.DivergenceAttributes)
}

// N6 (#433 review): the all-repos listing skips a repo that is not open at
// Debug, so a populating repo does not log a Warn on every GET /templates;
// a genuine failure on an OPEN repo still warns, and the one-repo listing
// still returns the error.
// SABOTAGE: log every skip at Warn → the not-open assertion red; log every
// skip at Debug → the genuine-error control red.
func TestListTemplates_NotOpenRepoIsNotAWarning(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	playbooksSource(t, m, root, "clone")
	half := m.newInstance("half-open", "uid-half-open", false)
	m.mu.Lock()
	m.repos["half-open"] = half
	m.mu.Unlock()
	t.Cleanup(func() {
		m.mu.Lock()
		delete(m.repos, "half-open")
		m.mu.Unlock()
	})
	_, _, err := half.Acquire()
	require.Error(t, err, "fixture: the repo is not open")

	logs := captureLogs(t, zerolog.DebugLevel)
	for range 2 {
		got, err := m.ListTemplates(context.Background(), "")
		require.NoError(t, err)
		require.Len(t, got, 4, "the open repo is still listed")
	}
	skips := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "repo skipped in the listing") && strings.Contains(line, `"repo":"half-open"`) {
			skips++
			require.Contains(t, line, `"level":"debug"`, line)
		}
	}
	require.Equal(t, 2, skips, "reached: the repo was skipped once per call")

	_, err = m.ListTemplates(context.Background(), "half-open")
	require.ErrorIs(t, err, ErrTemplateSourceUnavailable, "the one-repo listing still answers the error")

	// The level, by error: only the two "not there right now" sentinels are
	// Debug; a failure reading an open repo stays a Warn.
	require.Equal(t, zerolog.DebugLevel, listingSkipLevel(fmt.Errorf("%w: x", ErrTemplateSourceNotFound)))
	require.Equal(t, zerolog.WarnLevel, listingSkipLevel(fmt.Errorf("template source %q: read main: %w", "x", io.ErrUnexpectedEOF)))
	require.Equal(t, zerolog.WarnLevel, listingSkipLevel(fmt.Errorf("templates: blob: %w", plumbing.ErrObjectNotFound)))
}

// ── Modes ─────────────────────────────────────────────────────────────────

// F24 Verification "Modes" and "Source required": the request shape per mode.
func TestCreateFromTemplate_RequestShape(t *testing.T) {
	ref := &TemplateRef{Repo: "src", Name: "x"}
	for _, spec := range []CreateSpec{
		{Name: "a", Mode: "template"},
		{Name: "a", Mode: "template", Template: &TemplateRef{Name: "x"}},
		{Name: "a", Mode: "template", Template: ref, OntologyPreset: "default"},
		{Name: "a", Mode: "preset", OntologyPreset: "default", Template: ref},
		{Name: "a", Mode: "custom", OntologyYAML: "id: a", Template: ref},
		{Name: "a", Mode: "initialize", OntologyPreset: "default", Template: ref, Origin: &OriginSpec{URL: "x"}},
		{Name: "a", Mode: "clone", Template: ref, Origin: &OriginSpec{URL: "x"}},
		{Name: "a", Mode: "subscribe", Template: ref, Origin: &OriginSpec{URL: "x"}},
	} {
		err := validateOntologySources(spec)
		if spec.Mode == "clone" || spec.Mode == "subscribe" {
			err = rejectOntologySpecForClone(spec)
		}
		require.ErrorIs(t, err, ErrInvalidName, "%+v", spec)
	}
	require.ErrorContains(t, validateOntologySources(CreateSpec{Mode: "template", Template: &TemplateRef{Name: "x"}}), "template.repo")

	m := newTestManager(t)
	require.NoError(t, m.Start())
	_, err := m.Create(context.Background(), CreateSpec{Name: "a", Mode: "clone", Template: ref, Origin: &OriginSpec{URL: "x"}}, nil)
	require.ErrorIs(t, err, ErrInvalidName)
}

// F24 + user ruling 5: initialize + template follows today's initialize
// rules exactly. The template's tree lands as ONE signed, stamped commit on
// the agent branch only, replacing the remote's README.md there; the
// consensus branch is untouched; a branch that already has an ontology is
// refused (ErrRemoteAlreadyInitialized).
// SABOTAGE: fall back to the single ontology WriteFact → the tree and
// trailer assertions go red.
func TestCreateFromTemplate_InitializeWritesTreeOnAgentBranchOnly(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	playbooksSource(t, m, root, "clone")

	remote := sourceRepo(t, root, "project", sourceSpec{noOntol: true, files: map[string]string{
		"README.md": "# the project's own README\n", "src/main.go": "package main\n",
	}})
	bare := bareOf(remote)
	mainBefore := refHash(t, bare, "refs/heads/main")
	ri, err := m.Create(context.Background(), CreateSpec{Name: "proj", Mode: "initialize",
		Template: &TemplateRef{Repo: "playbooks", Name: "mission"}, Origin: &OriginSpec{URL: remote}}, nil)
	require.NoError(t, err)
	require.Equal(t, "mission", ri.Ontology().ID)
	require.Equal(t, mainBefore, refHash(t, bare, "refs/heads/main"), "the consensus branch is never written")

	agent := "refs/heads/" + tmplAgent
	msg := gitOut(t, bare, "log", "-1", "--format=%B", agent)
	require.Equal(t, "mission", store.TrailerValue(msg, store.TrailerTemplate))
	got := treeBlobs(t, bare, agent)
	for p, blob := range templateDirBlobs(t, "mission") {
		require.Equal(t, blob, got[p], "%s on the pushed agent branch", p)
	}
	require.Contains(t, got, "src/main.go", "the branch's other files stay")
	key, signed, err := testsigner.CommitSignerKey(bare, agent)
	require.NoError(t, err)
	require.True(t, signed)
	require.Equal(t, testsigner.Named("tmpl-host").PublicKey().Marshal(), key.Marshal())

	// Already a knowledge base: refused as initialize + preset is.
	kbRemote := seedBareRemote(t, filepath.Join(root, "kb-remote.git"))
	_, err = m.Create(context.Background(), CreateSpec{Name: "proj2", Mode: "initialize",
		Template: &TemplateRef{Repo: "playbooks", Name: "mission"}, Origin: &OriginSpec{URL: kbRemote}}, nil)
	require.ErrorIs(t, err, ErrRemoteAlreadyInitialized)
}

// ── Fleet ─────────────────────────────────────────────────────────────────

// F24 (user ruling 4): the fleet template creates the fleet repository only
// with initialize on the fleet's git URL — refused as a local repo, and
// refused while this instance already has a fleet. A repo created from it is
// the instance's fleet (IsFleetOntology).
func TestCreateFromTemplate_Fleet(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	playbooksSource(t, m, root, "clone")
	ref := &TemplateRef{Repo: "playbooks", Name: "fleet"}

	_, err := m.Create(context.Background(), CreateSpec{Name: "fleet", Mode: "template", Template: ref}, nil)
	require.ErrorIs(t, err, ErrTemplateFleetLocal)
	require.NotErrorIs(t, err, ErrTemplateFleetPresent, "a local fleet template is told to use initialize")

	fleetURL := sourceRepo(t, root, "fleet", sourceSpec{noOntol: true, files: map[string]string{"x.txt": "x"}})
	spec := CreateSpec{Name: "fleet", Mode: "initialize", Template: ref, Origin: &OriginSpec{URL: fleetURL}}

	// N2 (#433 review): a fleet registration or unregistration in flight
	// refuses the initialize, from preflight and from Create, and names the
	// state: a fleet repo is not mounted yet, so only the in-flight check can
	// refuse it.
	// SABOTAGE: drop the in-flight case → both rows red; keep only
	// registering → the unregistering row red.
	db := m.ControlDB()
	_, err = resolveIdentity(db, tmplAgent, fixedNow())
	require.NoError(t, err)
	for _, state := range []string{FleetRegistering, FleetUnregistering} {
		require.NoError(t, setFleetState(db, state, fixedNow()))
		row, err := m.fleetDB()
		require.NoError(t, err)
		require.Equal(t, state, row.State, "reached: the row the check reads is %s", state)
		require.Nil(t, m.fleetRepo())
		for _, err := range []error{m.CreatePreflight(context.Background(), spec), func() error {
			_, err := m.Create(context.Background(), spec, nil)
			return err
		}()} {
			require.ErrorIs(t, err, ErrTemplateFleetPresent, state)
			require.ErrorIs(t, err, ErrTemplateFleetLocal, "every fleet refusal still matches ErrTemplateFleetLocal")
			require.ErrorContains(t, err, "a fleet registration is "+state)
		}
		require.Nil(t, m.Get("fleet"))
	}

	// Added in review: a fleet state that cannot be READ refuses (fail
	// closed) with its own sentinel, never as a fleet that is present.
	// SABOTAGE: restore `err == nil &&` (fail open) → this row creates the
	// fleet → red.
	_, err = db.Exec(`ALTER TABLE instance_identity RENAME TO instance_identity_away`)
	require.NoError(t, err)
	_, err = m.Create(context.Background(), spec, nil)
	require.ErrorIs(t, err, ErrTemplateFleetStateUnavailable)
	require.NotErrorIs(t, err, ErrTemplateFleetLocal)
	require.Nil(t, m.Get("fleet"))
	_, err = db.Exec(`ALTER TABLE instance_identity_away RENAME TO instance_identity`)
	require.NoError(t, err)

	require.NoError(t, setFleetState(db, FleetStandalone, fixedNow()))
	ri, err := m.Create(context.Background(), CreateSpec{Name: "fleet", Mode: "initialize", Template: ref, Origin: &OriginSpec{URL: fleetURL}}, nil)
	require.NoError(t, err)
	require.True(t, fact.IsFleetOntology(ri.Ontology()))
	require.True(t, m.IsFleetRepo("fleet"))

	other := sourceRepo(t, root, "fleet2", sourceSpec{noOntol: true, files: map[string]string{"x.txt": "x"}})
	_, err = m.Create(context.Background(), CreateSpec{Name: "fleet2", Mode: "initialize", Template: ref, Origin: &OriginSpec{URL: other}}, nil)
	require.ErrorIs(t, err, ErrTemplateFleetLocal, "a second fleet repository is refused")
	require.ErrorIs(t, err, ErrTemplateFleetPresent, "and as a fleet that is present, not as a mode error")
	require.ErrorContains(t, err, `its fleet repository is "fleet"`)
}

// ── Listing ───────────────────────────────────────────────────────────────

// F24 Verification "Listing" + user ruling 2: two mounted repos with a
// `mission` template each are both listed, each with its repo, commit, fact
// and the fact's title; the per-repo listing returns one repo's; a repo with
// no templates lists nothing; a folder without a fact is not listed but IS
// creatable; a fact without a folder and a name with two facts are not
// listed. SABOTAGE: list a fact without checking its folder; read the agent
// branch (an agent-only template would appear).
func TestListTemplates(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	playbooksSource(t, m, root, "clone")
	onto := "id: t\nname: T\ntopics:\n  notes:\n    description: N\n"
	other := mount(t, m, "team", "clone", sourceRepo(t, root, "team", sourceSpec{files: map[string]string{
		".knomit/templates/mission/.knomit/ontology.yaml":     onto,
		"kb/templates/mission/aaaa1111.md":                    templateFactBody("mission", "Team mission"),
		".knomit/templates/folder-only/.knomit/ontology.yaml": onto,
		"kb/templates/fact-only/bbbb2222.md":                  templateFactBody("fact-only", "no folder"),
		".knomit/templates/twice/.knomit/ontology.yaml":       onto,
		"kb/templates/twice/cccc3333.md":                      templateFactBody("twice", "one"),
		"kb/templates/twice/dddd4444.md":                      templateFactBody("twice", "two"),
	}}))
	createRepo(t, m, "plain")
	svc, release, err := other.Acquire()
	require.NoError(t, err)
	_, _, err = svc.WriteSystemTree(context.Background(), other.AgentBranch(), map[string]string{
		".knomit/templates/agent-only/.knomit/ontology.yaml": onto,
	}, "agent only", "updated")
	release()
	require.NoError(t, err)

	all, err := m.ListTemplates(context.Background(), "")
	require.NoError(t, err)
	var keys []string
	for _, ti := range all {
		keys = append(keys, ti.Repo+"/"+ti.Name)
		require.Len(t, ti.Commit, 40)
		require.True(t, strings.HasPrefix(ti.Fact, "kb/templates/"+ti.Name+"/"), ti.Fact)
	}
	require.Equal(t, []string{"playbooks/coding", "playbooks/fleet", "playbooks/general", "playbooks/mission", "team/mission"}, keys)
	require.Equal(t, "Team mission", all[4].Description)
	require.True(t, strings.HasPrefix(all[3].Description, "The mission template"), all[3].Description)

	one, err := m.ListTemplates(context.Background(), "team")
	require.NoError(t, err)
	require.Len(t, one, 1)
	none, err := m.ListTemplates(context.Background(), "plain")
	require.NoError(t, err)
	require.Empty(t, none)

	_, err = m.Create(context.Background(), CreateSpec{Name: "from-folder", Mode: "template", Template: &TemplateRef{Repo: "team", Name: "folder-only"}}, nil)
	require.NoError(t, err, "a folder without a fact is creatable by name")
}

// N10: a create racing a rename and an archive of its source ends in a
// success or a named error, never a hang or a panic (run under -race).
func TestCreateFromTemplate_SourceRenamedDuringCreate(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	playbooksSource(t, m, root, "clone")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Create(context.Background(), CreateSpec{Name: "r" + string(rune('a'+i)), Mode: "template",
				Template: &TemplateRef{Repo: "playbooks", Name: "general"}}, nil)
		}(i)
	}
	_ = m.RenameRepo("playbooks", "renamed")
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			require.True(t, errors.Is(err, ErrTemplateSourceNotFound) || errors.Is(err, ErrTemplateSourceUnavailable), "%v", err)
		}
	}
}

var _ = plumbing.ZeroHash
