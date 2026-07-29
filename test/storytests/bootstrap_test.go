// Category R — Recovery. The acceptance story for backup + bootstrap: a home is
// populated with real facts and replicated, then DELETED, then rebuilt from the
// replica alone.
//
// These are slow by design. Every boot spawns a real knomit-backup child
// process, every phase drives real litestream replication against a file://
// replica, and the facts go through the production write/index path. There is no
// mock anywhere in the chain, because every bug this feature was built to
// prevent lives in the seams between the pieces rather than inside any one of
// them.
package storytests

import (
	"os"
	"path/filepath"
	"testing"

	"knomit/test/testenv"
	"knomit/test/testenv/backupenv"
)

// recoveryAgentName is the configured identity every recovery scenario boots
// with. It must NOT be this machine's hostname — the assertions below turn on
// the difference, because a branch derived from the hostname is stable on one
// machine and would make a hostname-derivation regression invisible.
const recoveryAgentName = "acceptance-instance-1"

// ── R1 ────────────────────────────────────────────────────────────────────

// TestRecovery_WipedHomeIsRebuiltFromTheReplicaAlone is the acceptance test for
// the whole feature.
//
// Four phases, and each one exists because the phase before it is not enough on
// its own:
//
//  1. Populate two repos with facts and replicate them.
//  2. DELETE the home — everything but the SSH key, which in production is a
//     mounted secret rather than volume state.
//  3. Boot again with nothing but the replica and that key, and assert the
//     facts come back with IDENTICAL CONTENT, on the SAME AGENT BRANCH, with
//     the restored commits reachable as history the new instance extends.
//  4. Write something new, destroy the home a SECOND time, and recover again.
//
// Phase 3's branch assertion is the point of the exercise. The bug that
// motivated this feature is not "the data is missing" — it is a byte-perfect
// restore that comes up writing to a DIFFERENT branch, leaving every restored
// commit stranded on a ref nothing reads and every new write on a ref nothing
// restored. The corpus is intact and invisible. A test that checked only the
// facts would pass against that.
//
// Phase 4 exists because recovery that works once is not recovery. An instance
// that comes up from a replica but does not itself replicate looks perfectly
// healthy until the second incident.
func TestRecovery_WipedHomeIsRebuiltFromTheReplicaAlone(t *testing.T) {
	t.Log("R1: populate → wipe the home → rebuild from the replica → wipe again → rebuild again")

	home := t.TempDir()
	replica := t.TempDir()
	backupenv.InjectAgentKey(t, home)
	key := backupenv.SaveAgentKey(t, home)

	// What this machine WOULD call itself with no agent_name configured. Every
	// identity assertion below is against this, not merely against "the same as
	// last time": on a single machine a hostname-derived branch is also stable,
	// so a same-as-last-time check cannot tell the fix from the bug.
	machineBranch := backupenv.MachineDerivedBranch(t, key)
	machineName, machineFingerprint := backupenv.SplitAgentBranch(t, machineBranch)
	if machineName == recoveryAgentName {
		t.Fatalf("this machine's hostname is %q, the same as the configured agent name — "+
			"the identity assertions below cannot distinguish machine-derived from config-derived "+
			"identity on this host; change recoveryAgentName", machineName)
	}

	// --- Phase 1: a populated, replicating instance -------------------------

	env := backupenv.New(t, backupenv.Opts{
		Home: home, Replica: replica,
		AgentName: recoveryAgentName, AgentPath: backupAgentBin,
	})
	env.CreateRepo("core")
	env.CreateRepo("notes")

	// Two repos, because the restore is driven by the registry inside
	// control.db rather than by anything on the volume: a bug that restored
	// only the first intended repo would survive a single-repo scenario.
	coreFacts := map[string]testenv.FactSpec{
		"kb/invariants/backup/replica-is-truth.md": testenv.Fact("the replica is the only truth after a wipe").
			Body("A restored volume knows nothing the replica did not carry. " +
				"control.db is what names the repos; without it every repo backup is orphaned.").
			Confidence(0.9).Domain("backup"),
		"kb/gotchas/backup/branch-fork.md": testenv.Fact("a restore onto a different branch is silent").
			Body("The corpus is intact and invisible: restored commits sit on a ref nothing reads.").
			Confidence(0.8).Domain("backup", "identity"),
	}
	notesFacts := map[string]testenv.FactSpec{
		"kb/conventions/testing/second-repo.md": testenv.Fact("the second repo proves the registry drove the restore").
			Body("Restoring only the first intended repo would pass a single-repo scenario.").
			Confidence(0.7).Domain("testing"),
	}

	for path, spec := range coreFacts {
		env.Learn("core", path, spec, "seed "+path)
	}
	for path, spec := range notesFacts {
		env.Learn("notes", path, spec, "seed "+path)
	}

	branchBefore := env.AgentBranch()
	coreHeadBefore := env.HeadCommit("core")
	notesHeadBefore := env.HeadCommit("notes")

	// Capture the exact serialized text. "The facts came back" has to mean
	// byte-identical content, not merely a row with the right path.
	wantCore := map[string]string{}
	for path := range coreFacts {
		wantCore[path] = env.FactContent("core", path)
		if wantCore[path] == "" {
			t.Fatalf("fixture: %s is empty before the wipe, so the content assertion would prove nothing", path)
		}
	}
	wantNotes := map[string]string{}
	for path := range notesFacts {
		wantNotes[path] = env.FactContent("notes", path)
	}

	// Fails fast and loudly if replication never started at all; Shutdown is
	// what actually guarantees the replica is complete.
	env.WaitReplicated("control", "core", "notes")
	env.Shutdown()

	// --- Phase 2: total loss ------------------------------------------------

	env.DestroyHome()
	assertReplicaHasContent(t, replica)

	// --- Phase 3: rebuild from the replica alone ----------------------------

	env2 := backupenv.New(t, backupenv.Opts{
		Home: home, Replica: replica,
		AgentName: recoveryAgentName, AgentPath: backupAgentBin,
	})

	// Identity first: everything after this is meaningless if the instance is
	// writing to a different branch than the one it just restored.
	if got := env2.AgentBranch(); got != branchBefore {
		t.Fatalf("agent branch changed across the recovery: %q → %q. Every restored commit is now "+
			"stranded on a ref this instance never writes to, and every new write lands on a ref "+
			"nothing restored — the silent fork this feature exists to prevent", branchBefore, got)
	}
	gotName, gotFingerprint := backupenv.SplitAgentBranch(t, env2.AgentBranch())
	if gotName != recoveryAgentName {
		t.Errorf("the recovered instance's branch names %q, want the CONFIGURED agent_name %q "+
			"(this machine would call itself %q — identity must not come from the machine, or a "+
			"restore onto different hardware forks the corpus)", gotName, recoveryAgentName, machineName)
	}
	if gotFingerprint != machineFingerprint {
		t.Errorf("branch fingerprint = %q, want %q — the identity's key half must come from the "+
			"INJECTED key, not from one minted at boot", gotFingerprint, machineFingerprint)
	}

	if _, err := os.Stat(filepath.Join(home, "control.db")); err != nil {
		t.Fatalf("control.db was not restored: %v — the registry inside it is the only record of "+
			"which repo databases should exist", err)
	}

	// Content, not existence.
	for path, want := range wantCore {
		if got := env2.FactContent("core", path); got != want {
			t.Errorf("core/%s came back with different content after the wipe:\n got: %q\nwant: %q", path, got, want)
		}
	}
	for path, want := range wantNotes {
		if got := env2.FactContent("notes", path); got != want {
			t.Errorf("notes/%s came back with different content after the wipe:\n got: %q\nwant: %q", path, got, want)
		}
	}

	// The restored commits are the branch's history, not something beside it.
	if got := env2.HeadCommit("core"); got != coreHeadBefore {
		t.Errorf("core: agent branch head = %q, want %q — the restored history is not what this "+
			"instance's write branch points at", got, coreHeadBefore)
	}
	if got := env2.HeadCommit("notes"); got != notesHeadBefore {
		t.Errorf("notes: agent branch head = %q, want %q", got, notesHeadBefore)
	}

	// The search index lives inside the same database as the facts, so it comes
	// back with them or the knowledge base is present but unusable. The query is
	// empty on purpose: the story harness embeds with a hash stub, so an empty
	// query (list every indexed fact on the branch) is the assertion that has
	// meaning, and semantic ranking is Category I's business.
	//
	// BOTH repos, not just the first. `notes` exists precisely to prove the
	// restore is registry-driven, and checking only `core` would let a bug that
	// affected the second registered repo — a loop that restores one and stops,
	// an index that is rebuilt only for the repo the manager opened first —
	// through untouched.
	for repo, wantFacts := range map[string]map[string]string{"core": wantCore, "notes": wantNotes} {
		indexed := env2.SearchPaths(repo, "")
		for path := range wantFacts {
			if !contains(indexed, path) {
				t.Errorf("%s/%s is not in the restored search index (indexed: %v) — the facts came "+
					"back but the corpus is not searchable", repo, path, indexed)
			}
		}
	}

	// --- Phase 4: the recovered instance must itself be replicating ----------

	const newPath = "kb/incidents/backup/second-wipe.md"
	newSpec := testenv.Fact("a write made after the first recovery").
		Body("Recovery that works once is not recovery: an instance that comes up from a replica " +
			"but does not itself replicate looks healthy until the second incident.").
		Confidence(0.95).Domain("backup")
	env2.Learn("core", newPath, newSpec, "written after recovery")
	wantNew := env2.FactContent("core", newPath)

	// The new write EXTENDS the restored history rather than starting beside it.
	if got := env2.HeadCommit("core"); got == coreHeadBefore {
		t.Fatal("the post-recovery write did not move the agent branch head")
	}
	for path, want := range wantCore {
		if got := env2.FactContent("core", path); got != want {
			t.Errorf("core/%s stopped resolving after a post-recovery write — the new commit did not "+
				"extend the restored tree", path)
		}
	}
	// The untouched repo must be exactly where the restore left it. A write to
	// one repo that moves another repo's branch head is a shared-state bug that
	// only a second repo can see.
	if got := env2.HeadCommit("notes"); got != notesHeadBefore {
		t.Errorf("notes: agent branch head = %q after a write to `core`, want %q — an untouched repo "+
			"moved", got, notesHeadBefore)
	}

	env2.WaitReplicated("control", "core", "notes")
	env2.Shutdown()
	env2.DestroyHome()

	env3 := backupenv.New(t, backupenv.Opts{
		Home: home, Replica: replica,
		AgentName: recoveryAgentName, AgentPath: backupAgentBin,
	})
	if got := env3.AgentBranch(); got != branchBefore {
		t.Fatalf("agent branch changed across the SECOND recovery: %q → %q", branchBefore, got)
	}
	if got := env3.FactContent("core", newPath); got != wantNew {
		t.Errorf("the fact written after the first recovery did not survive the second wipe:\n got: %q\nwant: %q",
			got, wantNew)
	}
	for path, want := range wantCore {
		if got := env3.FactContent("core", path); got != want {
			t.Errorf("core/%s did not survive the second wipe", path)
		}
	}
	for path, want := range wantNotes {
		if got := env3.FactContent("notes", path); got != want {
			t.Errorf("notes/%s did not survive the second wipe", path)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// assertReplicaHasContent is the fixture guard for the wipe: if the replica is
// empty there is nothing to recover FROM, and every assertion after the wipe
// would be testing an empty round trip.
func assertReplicaHasContent(t *testing.T, replica string) {
	t.Helper()
	var files int
	err := filepath.WalkDir(replica, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk replica %s: %v", replica, err)
	}
	if files == 0 {
		t.Fatalf("the replica at %s is empty after a full shutdown — nothing was ever replicated, "+
			"so the recovery below would prove nothing", replica)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
