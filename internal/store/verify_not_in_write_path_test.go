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
	forbidden := regexp.MustCompile(`\b(CheckRange|Audit|LoadFleet|FleetMembersAt|loadMembers|verifyCommitSignature|checkM3|checkOwnLineage|judgeCommit)\(`)
	for _, f := range []string{"fact_write.go", "sign.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := forbidden.FindAll(src, -1); len(m) > 0 {
			t.Errorf("%s calls verification in the write path: %q", f, m)
		}
	}
}
