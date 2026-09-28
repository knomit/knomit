package claude

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"knomit/internal/repos"
	"knomit/tools/bridge/knomitapi"
	"knomit/tools/bridge/skills"
)

//go:embed all:templates
var templatesFS embed.FS

// runInit scaffolds CC-side integration files into the current directory.
//
// Semantics:
//   - Owned files (.claude/skills/**): always written (overwritten if they
//     already exist).
//   - Merge-required files (.mcp.json, .claude/settings.json, CLAUDE.md): if
//     the destination exists it is MERGED in place — see init_merge.go. A
//     companion file is dropped only for the cases a merge cannot decide
//     unambiguously (a knomit .mcp.json entry under a different key, a CLAUDE.md
//     block with no closing delimiter).
//   - With neither --repo nor --lens the .mcp.json entry is UNBOUND: its args
//     carry no flag and the agent binds with knomit_bind. The directory name
//     appears only in the key (knomitapi.UnboundServerKey). If .mcp.json
//     already has a knomit entry, a flagless init keeps it as it is.
func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := flags.String("repo", "", "knomit repo name; omit both --repo and --lens for an unbound server the agent binds with knomit_bind")
	lens := flags.String("lens", "", "lens name; writes a lens-scoped .mcp.json (mutually exclusive with --repo)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *lens != "" && *repo != "" {
		return fmt.Errorf("--lens and --repo are mutually exclusive")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoName := *repo
	unbound := *repo == "" && *lens == ""

	// Validate names against the server's grammar BEFORE writing any file, so a
	// value containing quotes, backslashes, or other JSON-hostile characters is
	// rejected up front rather than silently baked into a broken .mcp.json. The
	// grammar lives in internal/repos (single source of truth); the bridge does
	// not duplicate it.
	//
	// Only an explicit name is validated. An unbound scaffold names nothing: the
	// directory name is sanitized into the key and never fails init.
	const nameRule = "must be lowercase letters, digits, hyphens, or underscores"
	serverKey := knomitapi.ServerKey(repoName, *lens)
	switch {
	case unbound:
		serverKey = knomitapi.UnboundServerKey(filepath.Base(cwd))
	case *lens != "":
		if !repos.IsValidName(*lens) {
			return fmt.Errorf("invalid --lens %q (%s)", *lens, nameRule)
		}
	default:
		if !repos.IsValidName(repoName) {
			return fmt.Errorf("invalid --repo %q (%s)", repoName, nameRule)
		}
	}
	// Reject up front rather than emit a .mcp.json whose derived key produces a
	// tool name over the API's 64-char limit.
	if !unbound && len(serverKey) > knomitapi.MaxServerKeyLen {
		scope, flag := repoName, "--repo"
		if *lens != "" {
			scope, flag = *lens, "--lens"
		}
		return fmt.Errorf("derived MCP server key %q is %d characters (max %d), "+
			"because %s name %q is %d (max %d); pass a shorter %s",
			serverKey, len(serverKey), knomitapi.MaxServerKeyLen,
			strings.TrimPrefix(flag, "--"), scope, len(scope), knomitapi.MaxScopeNameLen, flag)
	}

	// Merge targets are validated BEFORE anything is written, for the same reason
	// the name checks above are: a scaffold that half-lands leaves the operator
	// reconciling the files that merged against an error about the one that did
	// not. settings.json is the only one that can fail here — the other two fall
	// back to a companion when they cannot be merged, and a companion is not a
	// failure.
	// filepath.Join because this one IS opened; settingsRel because it must be
	// the same file mergeInto splices, and spelling it out again here is the
	// fourth restatement the constant exists to remove.
	if err := preflightSettings(filepath.Join(cwd, settingsRel)); err != nil {
		return err
	}

	// A flagless init keeps an existing knomit config that names ONE usable
	// scope, or is already unbound — knomitapi.SingleScope, the rule the
	// Antigravity host shares:
	// unbound is for a fresh scaffold. Without this, the same-key merge would
	// rewrite `--repo <dir>` under knomit-repo-<dir> to []. A broken or
	// ambiguous config is replaced by the unbound entry only when every knomit
	// entry is under init's own key; init never edits another key (the legacy
	// `knomit` one included), so otherwise it writes the companion instead.
	var kept, replaced string
	var mcpCompanion bool
	if unbound {
		kept, replaced, mcpCompanion = flaglessMcpPlan(filepath.Join(cwd, ".mcp.json"), serverKey)
	}

	var created []string
	var overwritten []string
	var updated []string
	var conflicts []string

	// writeOne handles one template file: srcFS/srcPath -> its destination,
	// applying the owned-vs-merge-required rules. Shared by both walks.
	writeOne := func(srcFS fs.FS, srcPath, dstRel string) error {
		data, err := fs.ReadFile(srcFS, srcPath)
		if err != nil {
			return err
		}
		// Only *.tmpl is templated; everything else is copied verbatim. A
		// literal `{{` in a SKILL.md is ordinary prose and must not abort a
		// scaffold half-written. See the same rule in the antigravity host.
		rendered := string(data)
		if strings.HasSuffix(srcPath, ".tmpl") {
			rendered, err = renderTemplate(string(data), map[string]string{
				"RepoName":  repoName,
				"Lens":      *lens,
				"ServerKey": serverKey,
			})
			if err != nil {
				return fmt.Errorf("render %s: %w", srcPath, err)
			}
		}
		dst := filepath.Join(cwd, dstRel)
		_, statErr := os.Stat(dst)
		exists := statErr == nil

		if isOwnedByIntegration(dstRel) {
			if err := writeFile(dst, []byte(rendered), 0o644); err != nil {
				return err
			}
			if exists {
				overwritten = append(overwritten, dstRel)
			} else {
				created = append(created, dstRel)
			}
			return nil
		}
		if exists {
			note := mergeNotPossible
			if dstRel != ".mcp.json" || !mcpCompanion {
				if note, err = mergeInto(dst, dstRel, []byte(rendered), serverKey); err != nil {
					return err
				}
			}
			if note == "" {
				// Merged and nothing changed: the file already says what this
				// build would have written. Deliberately no output — a re-init
				// that reports nothing is how the operator knows it is a no-op.
				return nil
			}
			if note == mergeNotPossible {
				if err := writeFile(companionPath(dst), []byte(rendered), 0o644); err != nil {
					return err
				}
				conflicts = append(conflicts, dstRel)
				return nil
			}
			updated = append(updated, strings.TrimSpace(dstRel+" "+note))
			return nil
		}
		if err := writeFile(dst, []byte(rendered), 0o644); err != nil {
			return err
		}
		created = append(created, dstRel)
		return nil
	}

	// Walk 1: this package's own templates (.mcp.json, settings.json, CLAUDE.md).
	err = fs.WalkDir(templatesFS, "templates", func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(srcPath, "templates/")
		// The three mcp.json templates all target .mcp.json — select exactly
		// one by scope, or none when a flagless init keeps an existing entry.
		switch rel {
		case "mcp.json.tmpl":
			if unbound || *lens != "" {
				return nil
			}
		case "mcp.json.lens.tmpl":
			if *lens == "" {
				return nil
			}
		case "mcp.json.unbound.tmpl":
			if !unbound || kept != "" {
				return nil
			}
		}
		dstRel := mapDestination(srcPath)
		if dstRel == "" {
			return nil // template excluded
		}
		return writeOne(templatesFS, srcPath, dstRel)
	})
	if err != nil {
		return err
	}

	// Walk 2: the shared skill templates, which live in their own package
	// because //go:embed cannot reach a sibling directory.
	err = fs.WalkDir(skills.FS, skills.Root, func(srcPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(srcPath, skills.Root+"/")
		return writeOne(skills.FS, srcPath, ".claude/skills/"+rel)
	})
	if err != nil {
		return err
	}

	printSummary(created, overwritten, updated, conflicts)
	if kept != "" {
		fmt.Printf("Kept: .mcp.json knomit entry (%s). To change it, edit or remove that entry in .mcp.json, then re-run init.\n", kept)
	}
	if replaced != "" {
		fmt.Printf("Replaced: .mcp.json knomit entry (%s) with an unbound entry\n", replaced)
	}
	return nil
}

// flaglessMcpPlan decides what a flagless init does to an existing .mcp.json:
// keep a single usable scope or an already-unbound config (under any key), replace a broken or ambiguous config under init's
// own key, or decline to a companion when another key is involved. It reads the
// file with knomitEntries — the merge's own lenient index — so it can never
// disagree with the merge about which entries exist.
func flaglessMcpPlan(path, serverKey string) (kept, replaced string, companion bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	entries, found := knomitEntries(data)
	if !found {
		return "", "", false
	}
	args := make([][]string, len(entries))
	ownKeyOnly := true
	for i, e := range entries {
		args[i] = e.args
		ownKeyOnly = ownKeyOnly && e.key == serverKey
	}
	s, skip := knomitapi.SingleScope(args, repos.IsValidName)
	switch {
	case skip == "":
		return s.String(), "", false
	case skip == knomitapi.SkipUnbound:
		// Already unbound is one scope, under any key. Going to the merge would
		// take its other-key path: a companion and a false two-scopes warning.
		return s.String(), "", false
	case ownKeyOnly:
		return "", knomitapi.ReplacedText(skip), false
	}
	return "", "", true
}

// preflightSettings reports a .claude/settings.json init cannot merge before it
// writes anything. The file is left exactly as it was: init never clobbers a file
// it cannot read, and never writes a companion for this one, because a companion
// beside a settings.json is precisely the silent failure that let the memory-guard
// hook go unregistered while the CLAUDE.md marker said v4.
//
// Two conditions, and the second is the one that bites. Unparseable is obvious.
// PARSEABLE BUT NOT AN OBJECT is not: `null`, `42`, `true`, a bare string and an
// array all index to a node whose span offsets are -1 (scalars have no
// delimiters), jsonNode.child returns nil for every lookup on them, and the
// splice helpers take those -1 offsets literally — `null` panics in lineIndent
// with "slice bounds out of range [:-1]". Catching it HERE rather than in
// mergeSettingsJSON is the point: the merge runs during the template walk, by
// which time CLAUDE.md and .mcp.json have already been rewritten, and a panic
// there leaves exactly the half-landed scaffold this preflight exists to prevent.
func preflightSettings(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // absent is fine; init will create it
		}
		// Anything else — a directory at that path, a permission error, bad I/O —
		// is NOT "absent". Treating it as absent passed the preflight and left the
		// failure to fire mid-walk, with CLAUDE.md and .mcp.json already rewritten:
		// the half-landed scaffold, reached through the check meant to prevent it.
		return fmt.Errorf("cannot merge %s: %w"+
			" (fix the file by hand, or move it aside and re-run init)",
			settingsRel, err)
	}
	if err := checkSettingsShape(data); err != nil {
		return fmt.Errorf("cannot merge %s: %w"+
			" (fix the file by hand, or move it aside and re-run init)",
			settingsRel, err)
	}
	return nil
}

// mergeNotPossible is the note a merge returns when it declines: the file is
// recognisable but the change is not unambiguous, so the companion protocol
// takes over. It is a sentinel rather than a bool so the three mergers share one
// return shape.
const mergeNotPossible = "\x00merge-not-possible"

// mergeInto merges the rendered template into an existing destination and
// reports what changed, as the parenthetical a summary line appends to the file
// name. An empty note means the file already agreed with the template and was
// not rewritten — which is what makes a second `init` byte-identical.
//
// An unparseable .claude/settings.json is the one case that ERRORS rather than
// falling back to a companion. The companion is silent-failure-prone by nature
// (the operator merges the files they noticed and the rest go stale), and
// settings.json is the file where going stale means a hook is simply not
// registered — the failure that motivated this whole change. So it is loud, and
// the file is left exactly as it was.
func mergeInto(dst, dstRel string, rendered []byte, serverKey string) (string, error) {
	existing, err := os.ReadFile(dst)
	if err != nil {
		return "", err
	}
	switch dstRel {
	case settingsRel:
		merged, added, err := mergeSettingsJSON(existing, rendered)
		if err != nil {
			return "", fmt.Errorf("cannot merge %s: %w "+
				"(fix the file by hand, or move it aside and re-run init)", dstRel, err)
		}
		if len(added) == 0 {
			return "", clearCompanion(dst)
		}
		if err := writeFile(dst, merged, 0o644); err != nil {
			return "", err
		}
		return "(+" + strings.Join(added, ", +") + ")", clearCompanion(dst)
	case "CLAUDE.md":
		merged, note, conflict := mergeClaudeMd(existing, rendered)
		if conflict {
			return mergeNotPossible, nil
		}
		if note == "" {
			return "", clearCompanion(dst)
		}
		if err := writeFile(dst, merged, 0o644); err != nil {
			return "", err
		}
		return note, clearCompanion(dst)
	case ".mcp.json":
		merged, note, conflict, err := mergeMcpJSON(existing, rendered, serverKey)
		if err != nil {
			return "", fmt.Errorf("cannot merge %s: %w", dstRel, err)
		}
		if conflict {
			return mergeNotPossible, nil
		}
		if note == "" {
			return "", clearCompanion(dst)
		}
		if err := writeFile(dst, merged, 0o644); err != nil {
			return "", err
		}
		return note, clearCompanion(dst)
	}
	return mergeNotPossible, nil
}

// clearCompanion removes the companion beside a file this run found MERGEABLE
// and left IN SYNC — whether the merge changed bytes or not.
//
// A companion exists for exactly one purpose: to carry init's rendering into a
// file init could not edit. Once the live file agrees with the template that
// purpose is spent, and what remains is a copy with no expiry, carrying whatever
// the template said when it was dropped. An operator who merges it later
// silently reverts the merge — or, for a file since merged BY HAND, applies a
// version older than the one already there. The hand-merged case is why the
// no-change path clears it too: that file will never have bytes to change again,
// so a changed-bytes-only rule would strand its companion permanently. That is
// not hypothetical; it is how this checkout ended up carrying one.
//
// Kept only when the run DECLINED — an unmergeable shape, a .mcp.json naming a
// knomit scope under another key, a CLAUDE.md block with no closing marker —
// where handing the operator the rendering is the whole point.
func clearCompanion(dst string) error {
	if err := os.Remove(companionPath(dst)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// isOwnedByIntegration reports whether dstRel is a file that the integration
// owns outright (skills). These are always overwritten on re-run, so deleting
// them and re-running init restores them.
func isOwnedByIntegration(dstRel string) bool {
	return strings.HasPrefix(dstRel, ".claude/skills/")
}

// settingsRel is the LOGICAL NAME of the settings file: repo-relative,
// forward-slash, identical on every platform. dstRel values are keys, not
// filesystem paths — mergeInto switches on this one, isSkill prefix-matches
// ".claude/skills/", and the single conversion to a real path happens at the
// boundary, `dst := filepath.Join(cwd, dstRel)` in writeOne. A path you OPEN is
// built with filepath.Join; a name you PRINT or MATCH is this.
//
// It is a constant because the drift is what caused the bug. preflightSettings
// built its message with filepath.Join instead — one site out of four — so on
// Windows a single `claude init` could report the same file two ways depending
// on which error fired:
//
//	cannot merge .claude\settings.json   (preflight)
//	cannot merge .claude/settings.json   (mergeInto)
//
// The switch case, the mapper and both messages now read the same constant, so
// they cannot drift apart again. Found by the Windows CI leg on the merge of
// #213 with the branch that added it, which is the first time #213's code ran
// on Windows at all.
const settingsRel = ".claude/settings.json"

// mapDestination translates a template path under templates/ to its
// destination inside the project. Returns "" if the file should not be copied.
// Skills are NOT handled here; they come from the shared skills package and are
// routed by the second walk in runInit.
func mapDestination(srcPath string) string {
	switch strings.TrimPrefix(srcPath, "templates/") {
	case "mcp.json.tmpl", "mcp.json.lens.tmpl", "mcp.json.unbound.tmpl":
		return ".mcp.json"
	case "CLAUDE-md-block.txt":
		return "CLAUDE.md"
	case "settings.json.tmpl":
		return settingsRel
	}
	return ""
}

func companionPath(dst string) string {
	if strings.HasSuffix(dst, "CLAUDE.md") {
		return dst + ".knomit-block"
	}
	return dst + ".knomit"
}

// renderTemplate is strict: templates are //go:embed-bundled and
// author-controlled, so a parse failure is a build-time bug, not a runtime
// condition to paper over. Return the error so init aborts loudly instead of
// shipping a file with unsubstituted {{.Var}} placeholders.
func renderTemplate(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{"jsonStr": jsonStr}).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// jsonStr renders s as a quoted JSON string literal (including the surrounding
// double quotes). Used by .mcp.json templates so any value substituted into the
// JSON is properly escaped — defense in depth behind name validation. Since the
// input is always a plain string, json.Marshal cannot fail here.
func jsonStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Unreachable for string input; fall back to an obviously-invalid token
		// so a hypothetical failure surfaces loudly rather than silently.
		return `""`
	}
	return string(b)
}

// writeFile replaces a file's contents atomically: it writes a temp file beside
// the target and renames over it, so a reader sees the old bytes or the new ones
// and never a truncated file. os.WriteFile opens with O_TRUNC, which is fine for
// a file init owns and regenerates, but init now writes IN PLACE to three files
// it does not own — an interrupted run would leave a user's CLAUDE.md empty with
// no backup.
//
// Symlinks are resolved first. A rename replaces the path, so a CLAUDE.md the
// user symlinked into another checkout would silently become a regular file and
// their real file would stop receiving updates. Writing through the link is what
// os.WriteFile did, and it has to survive the move to a rename.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".knomit-init-*")
	if err != nil {
		return err
	}
	// Removes the temp file on every failure path; a no-op once renamed away.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// CreateTemp makes the file 0600; restore the mode init asked for.
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func printSummary(created, overwritten, updated, conflicts []string) {
	if len(created) > 0 {
		fmt.Printf("Created: %s\n", strings.Join(created, ", "))
	}
	if len(overwritten) > 0 {
		fmt.Printf("Restored: %s\n", strings.Join(overwritten, ", "))
	}
	for _, u := range updated {
		fmt.Printf("Updated: %s\n", u)
	}
	for _, c := range conflicts {
		fmt.Printf("WARNING: %s exists — merge from %s manually\n", c, companionRel(c))
		switch c {
		case "CLAUDE.md":
			if note := claudeMdBlockNote(c); note != "" {
				fmt.Printf("         %s\n", note)
			}
		case ".mcp.json":
			// Deriving the key makes two knomit servers MERGEABLE, and the
			// obvious merge — both entries side by side — can disable the
			// hooks, since two DIFFERENT scopes leave no single repo to bind
			// to. (Two entries naming the same scope are fine; mcpBinding
			// treats them as one.) Say so here; the user finds out otherwise
			// only by noticing that knomit went quiet.
			fmt.Println("         note: keep ONE knomit SCOPE — a project whose knomit entries")
			fmt.Println("         name two different repos or lenses has no single repo to bind")
			fmt.Println("         to, so the hooks disable themselves (the MCP tools still work")
			fmt.Println("         for both). Duplicate entries naming the same scope are fine.")
		}
	}
}

// blockMarkerPrefix makes the integration block's version legible to init.
// Without a consumer the version in the marker would be inert decoration: a
// stale installed block would be indistinguishable from a current one, which is
// the whole reason the block drifted (installed copies saying "Nine /knomit-…
// slash commands" against a template saying "Eleven").
const (
	blockMarkerPrefix = "<!-- knomit:integration"
	// blockHeading identifies a block whose marker is missing entirely —
	// pre-marker installs, or a user who stripped the HTML comments.
	blockHeading = "## Working with knomit memory"
	// claudeMdBlockTemplate is the embedded block whose marker defines "current".
	claudeMdBlockTemplate = "templates/CLAUDE-md-block.txt"
)

// blockMarkerCurrent is the marker of the block THIS build ships, read off the
// embedded template rather than restated as a constant here. A hand-written copy
// drifts silently in BOTH directions: bump the template to v3 and forget the
// copy, and claudeMdBlockNote calls a freshly merged current block "from an
// older version"; bump the copy and forget the template, and every install is
// reported current forever. That is the same "the marker drifted because nothing
// read it" failure the marker exists to prevent, moved one level up.
//
// Empty if the template has no marker line. That disables only the
// already-current shortcut — claudeMdBlockNote guards against the empty string,
// since strings.Contains(anything, "") is true and would report every CLAUDE.md
// as current. TestBlockMarkerCurrent_ComesFromTemplate turns the condition into
// a test failure rather than a silent degradation.
var blockMarkerCurrent = templateBlockMarker()

func templateBlockMarker() string {
	data, err := templatesFS.ReadFile(claudeMdBlockTemplate)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(data), "\n")
	first = strings.TrimSpace(first)
	if !strings.HasPrefix(first, blockMarkerPrefix) {
		return ""
	}
	return first
}

// claudeMdBlockNote turns the classification of the CLAUDE.md already on disk
// into the advice its merge warning carries, so the warning says WHAT to merge.
// Returns "" when the file is unreadable or already current — nothing to add.
//
// It shares classifyClaudeMd with mergeClaudeMd on purpose: the advice printed
// and the merge attempted must never disagree about which block is there. Since
// init merges the cases it can, the only classification that still reaches this
// function from printSummary is blockUndelimited — a block with no closing
// marker, the one CLAUDE.md a merge cannot bound. The other arms stay because
// they are the same classification, stated as advice.
func claudeMdBlockNote(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	switch state, _, _, _ := classifyClaudeMd(string(data)); state {
	case blockCurrent:
		return ""
	case blockAbsent:
		return "it has no knomit block yet — append the companion's contents"
	default: // blockOlder, blockUndelimited
		return "its knomit block is from an older version — replace the whole block, not just parts"
	}
}

func companionRel(c string) string {
	if c == "CLAUDE.md" {
		return c + ".knomit-block"
	}
	return c + ".knomit"
}
