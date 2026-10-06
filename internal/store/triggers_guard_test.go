package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// triggersAllowedCalls is EVERY call the store half of the trigger dispatcher
// may make, keyed the way changes_guard_test.go keys them. An ALLOWLIST: the
// dispatcher's store surface is git READS, ONE read of the index's liveness
// join (DueCandidates: a SELECT over fact_expires ⋈ branch_facts) plus writes
// to its OWN three tables (trigger_watermarks, trigger_fires,
// trigger_due_fires) and nothing else — no ref move, no object write, no fact
// write, no commit-log read. A call that is not here fails the test; add it
// only after checking it keeps that contract. The one clock read is time.Now
// for fired_at, written for the operator, never read; time.Unix/UTC/Format
// render a stored stamp as RFC 3339 Z.
var triggersAllowedCalls = map[string][]string{
	"triggers.go": {
		// git reads
		"?.CommitObject", "c.Tree", "cur.NumParents", "tree.FindEntry", "tree.TreeEntryFile", "tree.File",
		"f.Contents", "walkHistory", "?.Reference", "?.IsFile", "ref.Hash", "?.String",
		"tt.commit", "tt.tree", "tt.blobHashAt", "rh.TreeReader", "?.Toucher", "?.BlobAt",
		"object.GetCommit", "ac.IsAncestor", "plumbing.NewBranchReferenceName", "verifyCommitSignature",
		"fact.OntologyPathsNewestFirst", "fact.TriggerScriptPath",
		// F07 PR 5: the recipe file at a commit (the dispatcher passes main's tip) and
		// the shared tree-entry reader; the fire-log reader shared by the recent
		// log and the lookup by run id
		"fact.TriggerRecipePath", "rh.privateFileAt", "rh.queryTriggerFires",
		// F23: GuidanceAt's path re-check and content checks — pure helpers
		// over the path string and the blob it read, no write
		"strings.HasPrefix", "path.Clean", "slices.Contains", "strings.Split", "utf8.ValidString", "strings.ContainsRune",
		// this store's own tables (and the read-only liveness join)
		"?.QueryContext", "rows.Next", "rows.Scan", "rows.Close", "rows.Err", "conn", "rh.branchID",
		"?.BeginTx", "tx.Rollback", "tx.Commit", "tx.ExecContext", "sb.WriteString", "sb.String", "rh.RecordTriggerRuns",
		// the signer
		"?.commitSigner", "signer.PublicKey", "cpk.CryptoPublicKey", "pki.Fingerprint",
		// pure helpers
		"boolInt", "UTCStamp", "time.Now", "time.Unix", "?.UTC", "?.Unix", "?.Format", "ctx.Err", "errors.Is", "errors.New", "fmt.Errorf", "sort.Strings",
		"append", "len", "make", "min",
	},
	"trailers.go": {
		// the reader, and the ctx transport of the trailer set (WithTrailers is
		// read by the three fact builders in fact_write.go, never here)
		"strings.TrimRight", "strings.LastIndex", "strings.Split", "strings.Cut", "strings.EqualFold", "strings.TrimSpace",
		"context.WithValue", "ctx.Value", "t.IsZero",
		// #349: WithAgentTrace's refusals and its ctx attach (WithTrailers, the
		// same transport), and appendTrailers sorting a COPY of the agent's
		// own entries — pure helpers, no write
		"errors.New", "WithTrailers", "len", "append", "sort.SliceStable",
		// rehearsal F10: rendering a set (t.lines) and joining it to a merge
		// commit's trailer paragraph (appendTrailersToParagraph) — string
		// helpers, no write
		"t.lines", "isTrailerParagraph", "appendTrailers",
	},
}

// TestTriggers_StoreCallsAreAllowlisted is the structural no-write guard for
// the dispatcher's store files, modelled on F05's. Sabotage: add a WriteFact
// (or any ref/object write) to triggers.go — the call is not on the list.
func TestTriggers_StoreCallsAreAllowlisted(t *testing.T) {
	fset := token.NewFileSet()
	for f, list := range triggersAllowedCalls {
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
				return true
			}
			seen[key] = true
			if !allowed[key] {
				t.Errorf("%s: call %s is not on the triggers allowlist — the dispatcher's store surface is git reads "+
					"plus its own two tables; check it and add it to triggersAllowedCalls only if it keeps that contract",
					fset.Position(call.Pos()), key)
			}
			return true
		})
		var unused []string
		for c := range allowed {
			if !seen[c] {
				unused = append(unused, c)
			}
		}
		sort.Strings(unused)
		require.Empty(t, unused, "%s: allowlist entries no longer called — remove them", f)
		require.Greater(t, len(seen), 5, "%s: the guard must actually have walked the file", f)
	}
}
