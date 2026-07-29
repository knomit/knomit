package storytests

import (
	"context"
	"testing"
	"time"

	"knomit/test/testenv"
	"knomit/test/testenv/backupenv"
)

// shutdownMonitorInterval is the replication poll for the scenario below, and
// the number the whole test turns on.
//
// The acceptance fixtures run at 50ms, which is right for them: they want
// replication to be live and are asserting about something else. Here the
// QUESTION is which teardown step carried the last write, so a background tick
// firing between the write and the shutdown would answer it for free and the
// test would pass whatever the shutdown did.
//
// Three seconds is the compromise. It has to be long enough that the tick after
// the one the test waits for cannot land inside the few hundred milliseconds
// between the write and backup.Manager.Close — which the test asserts directly,
// below, rather than trusting the arithmetic — and short enough that waiting for
// the FIRST tick, which the scenario does need, is not the slowest thing in the
// suite.
const shutdownMonitorInterval = 3 * time.Second

// TestRecovery_ProductionShutdownOrderFlushesTheLastWrite tears an instance down
// in the order the SHIPPED server uses and asserts the last write survives a
// wiped volume.
//
// Both acceptance fixtures — backupenv.Shutdown and internal/app's stopInstance
// — untrack every database before closing anything, which is the reverse of
// cmd/serve. Part of the reason given was a belief that no longer holds: that
// knomit's own close checkpoints and deletes each -wal out from under litestream,
// so the final sync fails and the error is discarded. That WAS true while
// litestream ran inside the knomit process, because POSIX advisory locks do not
// conflict between two descriptors held by the same process, so knomit's close
// could take the exclusive lock a checkpoint-and-delete needs. Running litestream
// in the knomit-backup child process removed the mechanism: the kernel arbitrates
// across processes, knomit's close cannot take that lock, the -wal survives it,
// and the agent reads it during the final sync.
//
// The consequence of the stale belief was that the shipped shutdown path had no
// coverage at all — every fixture went out of its way to avoid it. This is that
// coverage.
//
// What it pins, in cmd/serve's exact LIFO order:
//
//  1. repos.Manager.Close() — knomit's SQLite handles close first;
//  2. backup.Manager.Close() — the agent's final sync runs last, and returns
//     nil (asserted inside ShutdownInServerOrder);
//  3. the volume is destroyed and rebuilt from the replica alone, and the write
//     made just before step 1 comes back byte-identical.
//
// Step 3 is what makes step 2 mean anything: a Close that returned nil having
// flushed nothing would satisfy step 2 on its own.
func TestRecovery_ProductionShutdownOrderFlushesTheLastWrite(t *testing.T) {
	t.Log("R4: write → shut down in cmd/serve's order → wipe the home → the last write is still there")

	home := t.TempDir()
	replica := t.TempDir()
	backupenv.InjectAgentKey(t, home)

	opts := backupenv.Opts{
		Home: home, Replica: replica,
		AgentName: recoveryAgentName, AgentPath: backupAgentBin,
		MonitorInterval: shutdownMonitorInterval,
	}

	env := backupenv.New(t, opts)
	env.CreateRepo("core")

	// Wait for the FIRST replication of both databases before writing anything
	// that matters. Not belt and braces: litestream initialises a database on its
	// first monitor tick, and a database tracked and closed before that tick
	// replicates nothing at all — the shutdown sync has no chain to append to.
	// Waiting here also places the write as far from the NEXT tick as it can be.
	env.WaitReplicated("control", "core")
	replicatedBefore := remoteTXID(t, env, "core")

	const factPath = "kb/invariants/backup/final-sync-carries-the-last-write.md"
	env.Learn("core", factPath,
		testenv.Fact("the shutdown sync is what carries the writes since the last tick").
			Body("knomit's SQLite handles close before the agent's final sync. Across processes the "+
				"kernel arbitrates the WAL lock, so knomit's close cannot delete the -wal and the "+
				"agent still has something to read.").
			Confidence(0.9).Domain("backup"),
		"the last write before shutdown")

	want := env.FactContent("core", factPath)
	if want == "" {
		t.Fatal("fixture: the fact is empty before the shutdown, so the content assertion would prove nothing")
	}
	headBefore := env.HeadCommit("core")
	branchBefore := env.AgentBranch()

	// The assertion that makes everything below mean what it says: at this point
	// the replica must NOT already hold the write. If it does, a background tick
	// beat the shutdown to it and the recovery would succeed without the shutdown
	// having contributed anything.
	if got := remoteTXID(t, env, "core"); got != replicatedBefore {
		t.Fatalf("core reached replica txid %d before the shutdown (was %d): a monitor tick landed between "+
			"the write and the teardown, so this run proves nothing about the shutdown. Raise "+
			"shutdownMonitorInterval", got, replicatedBefore)
	}

	env.ShutdownInServerOrder()
	env.DestroyHome()

	next := backupenv.New(t, opts)
	if got := next.AgentBranch(); got != branchBefore {
		t.Fatalf("agent branch after recovery = %q, want %q", got, branchBefore)
	}
	if got := next.HeadCommit("core"); got != headBefore {
		t.Fatalf("core head after recovery = %q, want %q — the shutdown's final sync did not carry the last commit",
			got, headBefore)
	}
	if got := next.FactContent("core", factPath); got != want {
		t.Fatalf("fact content after recovery differs from what was written before the shutdown:\n got: %q\nwant: %q",
			got, want)
	}
	// The empty query lists every indexed fact on the branch, which is the
	// assertion with meaning here: the corpus came back USABLE, not merely
	// present. Ranking is another category's business.
	indexed := next.SearchPaths("core", "")
	found := false
	for _, p := range indexed {
		if p == factPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%s is not in the restored search index (indexed: %v) — the fact came back but the "+
			"corpus is not searchable", factPath, indexed)
	}
}

// remoteTXID is the transaction the replica currently holds for a database. It is
// how this test tells "the shutdown flushed this" from "a tick already had".
func remoteTXID(t *testing.T, env *backupenv.Env, name string) uint64 {
	t.Helper()
	for _, st := range env.Backup().Status(context.Background()) {
		if st.Name == name {
			return st.RemoteTXID
		}
	}
	t.Fatalf("no replication status for %q", name)
	return 0
}
