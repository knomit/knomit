package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// changesAllowedCalls is EVERY call the three changes files may make, keyed
// "<receiver>.<method>" ("?.<method>" when the receiver is itself an
// expression, the bare name for a plain function). It is an ALLOWLIST on
// purpose: a denylist of known writers passes any writer nobody thought to
// list (rh.CreateBranch and svc.Branches().CreateBranch both slipped through
// the first version). A call that is not here fails the test — add it only
// after checking it neither writes (refs, objects, SQLite rows) nor reads a
// clock or the commit log.
var changesAllowedCalls = map[string][]string{
	"changes.go": {
		// git reads
		"?.CommitObject", "c.IsAncestor", "sinceCommit.IsAncestor", "c.Tree", "root.Tree", "object.DiffTree",
		"rh.resolveRef", "rh.ontologyRoot", "plumbing.NewHash", "?.String",
		// pure helpers
		"NormalizeChangesPrefix", "diffSubtrees", "subtreeOrEmpty", "parseFullHash", "isFact",
		"fact.IsPrivatePath", "strings.Contains", "strings.ToLower", "strings.Trim", "sort.Slice",
		"errors.Is", "errors.New", "fmt.Errorf",
		"json.Marshal", "json.Unmarshal", "?.EncodeToString", "?.DecodeString",
		"append", "len", "make",
	},
	"../mcp/changes.go": {
		"repos.RequireBinding", "b.Write", "?.Acquire", "release", "svc.UpstreamBranch", "svc.Facts", "?.ChangesUnder",
		"store.DecodeChangesCursor", "store.EncodeChangesCursor",
		"req.GetInt", "req.GetString", "context.WithTimeout", "cancel",
		"mcpgo.NewTool", "mcpgo.WithDescription", "mcpgo.WithString", "mcpgo.WithNumber", "mcpgo.Description",
		"mcpgo.NewToolResultError", "mcpgo.NewToolResultText", "bindingArg",
		"changesErrorText", "errStoreUnavailable.Error", "err.Error", "errors.Is", "fmt.Sprintf", "json.Marshal",
		"len", "string",
	},
	"../web/handlers_changes.go": {
		"repos.RepoFromContext", "BranchFromContext", "chi.URLParam", "ri.WithRead", "svc.Facts", "?.ChangesUnder",
		"store.DecodeChangesCursor", "store.EncodeChangesCursor",
		"?.Query", "qp.Get", "r.Context", "nq.Del", "nq.Set", "nq.Encode", "strconv.Atoi", "strconv.Itoa",
		"b.Branch", "selfWithQuery", "hal.WriteHAL", "hal.WriteProblem", "writeChangesError", "writeStoreError",
		"aerr.Error", "err.Error", "errors.Is", "len",
	},
}

// TestChanges_CallsAreAllowlisted is the structural half of F05's contract:
// the changes read — store, MCP tool and REST route — is a read of two trees.
// It must never reach a store WRITE (a new write path is exactly what F05 was
// approved without) and never read a CLOCK or the commit log (commit time is
// set by whoever makes the commit, and ordering by it is how a deletion gets
// lost). The behavioural half is TestChangesUnder_WritesNothing here and
// TestChangesHandler_WritesNothing in internal/mcp.
func TestChanges_CallsAreAllowlisted(t *testing.T) {
	fset := token.NewFileSet()
	for f, list := range changesAllowedCalls {
		allowed := map[string]bool{}
		for _, c := range list {
			allowed[c] = true
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err, f)
		seen := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key := callKey(call)
			if key == "" {
				return true // a func-literal call; nothing named
			}
			seen[key] = true
			if !allowed[key] {
				t.Errorf("%s: call %s is not on the changes allowlist — the changes read must be two trees, "+
					"no write, no clock, no commit log; check it and add it to changesAllowedCalls if it is a pure read",
					fset.Position(call.Pos()), key)
			}
			return true
		})
		// Keep the allowlist honest: an entry nothing calls any more is a
		// hole waiting for a writer with that name.
		var unused []string
		for c := range allowed {
			if !seen[c] {
				unused = append(unused, c)
			}
		}
		sort.Strings(unused)
		require.Empty(t, unused, "%s: allowlist entries no longer called — remove them", f)
		require.Greater(t, len(seen), 15, "%s: the guard must actually have walked the file", f)
	}
}

func callKey(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name
		}
		return "?." + fn.Sel.Name
	}
	return ""
}

// storeSnapshot is everything a write could touch: every ref (name → target),
// the branches table, and the commit_log / branch_commits row counts.
type storeSnapshot struct {
	Refs                        map[string]string
	Branches                    []string
	CommitLog, BranchCommitRows int
}

func snapshotStore(t *testing.T, svc *Service) storeSnapshot {
	t.Helper()
	snap := storeSnapshot{Refs: map[string]string{}}
	iter, err := svc.rh.gits.IterReferences()
	require.NoError(t, err)
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		snap.Refs[r.Name().String()] = r.Hash().String() + " " + r.Target().String()
		return nil
	}))
	bs, err := svc.Branches().ListBranches(context.Background())
	require.NoError(t, err)
	for _, b := range bs {
		snap.Branches = append(snap.Branches, b.Name)
	}
	sort.Strings(snap.Branches)
	require.NoError(t, svc.rh.gits.DB().QueryRow(`SELECT COUNT(*) FROM commit_log`).Scan(&snap.CommitLog))
	require.NoError(t, svc.rh.gits.DB().QueryRow(`SELECT COUNT(*) FROM branch_commits`).Scan(&snap.BranchCommitRows))
	return snap
}

// TestChangesUnder_WritesNothing: every kind of call — a full read, an
// incremental one, a page, and each refusal — leaves every ref, the branches
// table and the commit-log tables exactly as they were.
func TestChangesUnder_WritesNothing(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	since := writeF(t, svc, "main", "kb/tasks/a/t1.md")
	writeF(t, svc, "main", "kb/tasks/a/t2.md")
	side := writeF(t, svc, "agent/a", "kb/tasks/a/side.md")

	before := snapshotStore(t, svc)
	require.NotEmpty(t, before.Refs)
	require.Greater(t, before.CommitLog, 0, "fixture: the commit log must be populated to show it does not grow")
	for _, q := range []ChangesQuery{
		{Prefix: "tasks/a"},
		{Prefix: "tasks/a", Since: since},
		{Prefix: "tasks/a", Since: since, Limit: 1},
		{Prefix: "tasks/a", Since: side},
		{Prefix: "tasks/a", Since: "0123456789abcdef0123456789abcdef01234567"},
		{Prefix: "../x"},
	} {
		_, _ = svc.Facts().ChangesUnder(ctx, "main", q)
	}
	_, _ = svc.Facts().ChangesUnder(ctx, "no-such-branch", ChangesQuery{})
	require.Equal(t, before, snapshotStore(t, svc))
}
