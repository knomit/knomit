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
// dispatcher's store surface is git READS plus writes to its OWN two tables
// (trigger_watermarks, trigger_fires) and nothing else — no ref move, no
// object write, no fact write, no commit-log read. A call that is not here
// fails the test; add it only after checking it keeps that contract. The one
// clock read is time.Now for fired_at, written for the operator, never read.
var triggersAllowedCalls = map[string][]string{
	"triggers.go": {
		// git reads
		"?.CommitObject", "c.Tree", "cur.NumParents", "tree.FindEntry", "tree.TreeEntryFile", "tree.File",
		"f.Contents", "walkHistory", "?.Reference", "?.IsFile", "ref.Hash", "?.String",
		"tt.commit", "tt.tree", "tt.blobHashAt", "rh.TreeReader", "?.Toucher", "?.BlobAt",
		"object.GetCommit", "ac.IsAncestor", "plumbing.NewBranchReferenceName", "verifyCommitSignature",
		"fact.OntologyPathsNewestFirst",
		// this store's own tables
		"?.QueryContext", "rows.Next", "rows.Scan", "rows.Close", "rows.Err", "conn",
		"?.BeginTx", "tx.Rollback", "tx.Commit", "tx.ExecContext", "sb.WriteString", "sb.String", "rh.RecordTriggerRuns",
		// the signer
		"?.commitSigner", "signer.PublicKey", "cpk.CryptoPublicKey", "pki.Fingerprint",
		// pure helpers
		"boolInt", "time.Now", "?.Unix", "ctx.Err", "errors.Is", "errors.New", "fmt.Errorf", "sort.Strings",
		"append", "len", "make",
	},
	"trailers.go": {
		"strings.TrimRight", "strings.LastIndex", "strings.Split", "strings.Cut", "strings.EqualFold", "strings.TrimSpace",
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
