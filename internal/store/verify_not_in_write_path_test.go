package store

import (
	"os"
	"regexp"
	"testing"
)

// TestVerifyIsNeverInTheWritePath (F09): verification runs once, at the gate
// that advances a knowledge base's main (CheckRange, in CI or on demand), and
// the audit runs only when asked. Nothing verify-related may be called from
// the fact write path or the signing chokepoint, so writing a fact costs
// exactly what it did before F09 (signing only).
func TestVerifyIsNeverInTheWritePath(t *testing.T) {
	forbidden := regexp.MustCompile(`\b(CheckRange|Audit|LoadFleet|LoadFleetAt|FleetMembersAt|FleetAt|loadMembers|LoadMembers|LoadMembersOfTree|LoadFleetAtCommit|loadFleetPKI|newJudge|NewJudge|verifyCommitSignature|VerifyCommitSignature|checkM3|CheckM3|checkOwnLineage|CheckOwnLineage|judgeCommit|JudgeCommit|judgeIncoming|JudgeIncoming|judgeOwnRange|JudgeOwnRange)\(`)
	// The name list above is bound to spellings: after the fleet read moved
	// into internal/store/fleetrec, fleetrec.At( reached the same read under
	// a name the list does not hold. Reject ANY package-qualified call into
	// the fleetrec package, so a new export cannot slip past the list.
	pkgCall := regexp.MustCompile(`\bfleetrec\.\w+\(`)
	// Likewise for internal/store/verify: any verify.<Export>( call is
	// verification, whether or not its name is in the list above.
	verifyPkgCall := regexp.MustCompile(`\bverify\.\w+\(`)
	// And for internal/store/remotesync, which holds the publish-time judge of
	// this instance's own commits (JudgeOwnRange): any call qualified by the
	// package is the reconcile/publish machinery, never the write path. The
	// package is named remotesync so the standard library's sync calls (for
	// example sync.OnceFunc) are not mistaken for it.
	syncPkgCall := regexp.MustCompile(`\bremotesync\.\w+\(`)
	for _, f := range []string{"facts/fact_write.go", "internal/vcs/sign.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := forbidden.FindAll(src, -1); len(m) > 0 {
			t.Errorf("%s calls verification in the write path: %q", f, m)
		}
		if m := pkgCall.FindAll(src, -1); len(m) > 0 {
			t.Errorf("%s calls into the fleetrec package from the write path: %q", f, m)
		}
		if m := verifyPkgCall.FindAll(src, -1); len(m) > 0 {
			t.Errorf("%s calls into the verify package from the write path: %q", f, m)
		}
		if m := syncPkgCall.FindAll(src, -1); len(m) > 0 {
			t.Errorf("%s calls into the remotesync package from the write path: %q", f, m)
		}
	}
}
