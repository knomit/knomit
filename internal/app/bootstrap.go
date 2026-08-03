package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/backup"
	"knomit/internal/config"
	"knomit/internal/repos"
)

// ErrIdentityRequired means a backup-enabled instance did not supply a stable
// identity. Both halves of the agent branch must be pinned: agent_name AND an
// injected SSH key. Generating either would make a restored container write to a
// DIFFERENT branch than the one its restored history lives on — a silent fork.
var ErrIdentityRequired = errors.New("backup-enabled instance requires agent_name and an injected SSH key")

// BootResult carries what Bootstrap resolved into app.New, so identity is
// derived exactly once rather than independently in two places.
type BootResult struct {
	Signer      ssh.Signer
	AgentBranch string
	// Backup is nil when replication is disabled. It is a *backup.Manager and
	// every method is nil-receiver-safe, but do NOT assign it straight into an
	// interface variable: a typed nil pointer in an interface is not a nil
	// interface. Guard the assignment (see app.New).
	Backup *backup.Manager

	// ReplicateControl reports whether control.db may be replicated on this
	// boot. It is false when its restore FAILED — as distinct from finding no
	// backup, which is an ordinary first boot.
	//
	// This is the one place the warm-start-cache framing does not hold, and the
	// exception is worth stating precisely. Every OTHER database here is
	// rebuildable from git, so replicating a fresh empty one over the replica
	// costs nothing that cannot be re-derived. control.db is not: the registry
	// is the only record of which repos exist and which origin each was cloned
	// from, and it lives nowhere else. Opening the registry CREATES an empty
	// control.db, so a failed restore leaves us holding an empty file that
	// litestream would re-anchor against the replica and snapshot as the new
	// head — destroying the very registry the restore failed to read, from a
	// transient error.
	//
	// So a boot that could not read the registry declines to write over it and
	// runs with replication of control.db off until a restart succeeds. Note
	// the zero value is the SAFE one: a BootResult that never reached the
	// restore does not replicate.
	ReplicateControl bool

	// bootstrapped is set only by Bootstrap, and checked by New. It is what
	// makes "restore before you open anything" an invariant rather than a
	// convention: a hand-built BootResult carries a plausible signer and branch
	// but no restore has run, so New would open databases onto a volume that
	// was never rehydrated — and replication would then write that empty state
	// over the good backup. Unexported, so no caller outside this package can
	// set it even by accident.
	bootstrapped bool
}

// BootstrapIdentity resolves the agent identity and NOTHING else: no replica
// client, no child process, no restore. It returns a BootResult that app.New
// accepts, with Backup nil and ReplicateControl false.
//
// It is the entry point for commands that must not touch the replica —
// `knomit verify` is the one that exists today. Verify is a read-only integrity
// check over what is on THIS volume, and full Bootstrap would have made it
// three things it should not be: a writer (restore fills absent databases under
// KNOMIT_HOME), a second litestream process against the same replica prefix as
// a running server, and a command that fails when a bucket is unreachable.
//
// The identity check is NOT relaxed here. It keys off backup.enabled rather
// than off whether this call opens the replica, because the hazard it guards is
// not about the replica at all: a generated key means a fresh agent branch, so
// anything that opens these repos and writes would write to a branch the
// restored history does not live on. That is as true of verify as of serve.
//
// Zero value note: BootResult.ReplicateControl stays false, which is the safe
// setting — nothing that goes through this function replicates anything.
func BootstrapIdentity(cfg config.Config) (*BootResult, error) {
	keyPath := keyPathFor(cfg)
	if cfg.Backup.Enabled {
		if cfg.AgentName == "" {
			return nil, fmt.Errorf("%w: agent_name is empty (set agent_name or KNOMIT_AGENT_NAME)", ErrIdentityRequired)
		}
		// Deliberately checked BEFORE ensureKeyPair rather than by asking it not
		// to generate: on a backup-enabled instance, generating a key is never a
		// tolerable fallback. A fresh fingerprint means a fresh agent branch, so
		// the restored history would sit on a branch nothing writes to while the
		// new writes land on a branch nothing restored — the silent fork this
		// whole feature exists to prevent, visible only as "my data vanished".
		if _, err := os.Stat(keyPath); err != nil {
			return nil, fmt.Errorf("%w: no SSH key at %s — mount it as a secret rather than letting one be generated",
				ErrIdentityRequired, keyPath)
		}
	}

	signer, keyFingerprint, err := ensureKeyPair(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ensure keypair: %w", err)
	}
	res := &BootResult{
		Signer:       signer,
		AgentBranch:  agentBranch(cfg.AgentName, keyFingerprint),
		bootstrapped: true,
	}
	log.Info().Str("agent_branch", res.AgentBranch).Msg("resolved agent identity")
	return res, nil
}

// Bootstrap prepares KNOMIT_HOME before any database is opened.
//
// The replica is a WARM-START CACHE, not durable state: git is the source of
// truth and every database here is rebuildable from it (see the
// git-is-the-only-source-of-truth principle). So nothing in this function
// refuses a boot. Every failure below degrades to the same outcome — the
// instance starts anyway and the repos it could not rehydrate are re-cloned
// from their recorded origins. A cache miss must never become an outage.
//
// The ordering is still load-bearing. Restore fills an ABSENCE and never
// overwrites, so anything that opens or creates a database before its restore
// has run turns that restore into a silent no-op — which costs the boot-time
// saving the cache exists to provide:
//
//  1. identity   — resolve agent_name + key. First because an instance with an
//     unstable identity would restore one branch's history and then
//     write to another — a silent fork, which no amount of
//     re-cloning fixes.
//  2. backup     — build the replica client. A failure here is logged and the
//     boot CONTINUES without the cache.
//  3. control.db — restore. Must precede step 4: opening the registry CREATES
//     control.db when it is absent, which would leave restore
//     nothing to fill. A restore that FAILS here (as opposed to
//     finding no backup) also switches this boot's control
//     replication off — see BootResult.ReplicateControl.
//  4. registry   — open control.db (this also runs migrate.Control) and read
//     the intended repo set. Must precede step 5: the registry is
//     the only record of which repo databases should exist — a
//     restored machine's repos/ directory is empty.
//  5. repos      — restore every intended database that is absent locally.
//
// Steps 3-5 all finish before app.New runs repos.Manager.Start, which is what
// opens the repo databases for real.
func Bootstrap(ctx context.Context, cfg config.Config) (*BootResult, error) {
	// 1. Identity. Shared with BootstrapIdentity, which is step 1 on its own —
	// see there for what the backup-enabled key check is really guarding.
	res, err := BootstrapIdentity(cfg)
	if err != nil {
		return nil, err
	}

	// 2. Backup client. A cache that cannot be reached must not stop the
	// service: the databases it holds are rebuildable from git, so the cost of
	// coming up without it is a slower boot and no replication until the next
	// restart — both of which are visible in the log, and neither of which is
	// data loss. Refusing here would turn a bad credential or an unreachable
	// bucket into an outage.
	bm, err := backup.Open(cfg.Backup, cfg.Home)
	if err != nil {
		log.Error().Err(err).Msg("backup: replica unavailable; starting WITHOUT it — " +
			"repos will be rebuilt from their origins, and nothing is being replicated until this is fixed")
		return res, nil
	}
	if bm == nil {
		return res, nil // replication disabled: nothing to restore
	}
	res.Backup = bm

	// Failures inside are logged, not returned — see restoreHome. The one bit
	// that comes back is whether control.db may be replicated: that is a guard
	// against overwriting good state, not a report of what went wrong.
	res.ReplicateControl = restoreHome(ctx, cfg, bm)
	return res, nil
}

// restoreHome runs steps 3-5: rehydrate control.db, read the intended repo set
// from it, and rehydrate every intended repo database that is absent.
//
// It reports ONE thing, and it is not an error report: whether control.db may
// be replicated on this boot. Every other failure here costs boot TIME and
// nothing else — a repo that could not be rehydrated is re-cloned from the
// origin its registry row records, which is what repos.Manager.Start does with
// any registered repo whose database is missing — so reporting those upwards
// would only give a caller the chance to refuse a boot that should not be
// refused. The registry is the exception, for the reason spelled out on
// BootResult.ReplicateControl.
func restoreHome(ctx context.Context, cfg config.Config, bm *backup.Manager) (replicateControl bool) {
	controlPath := filepath.Join(cfg.Home, "control.db")

	if err := bm.RestoreControl(ctx); err != nil {
		// Without control.db there is no registry, so there is no intended repo
		// set to restore and nothing below can run. The server still starts: it
		// comes up with whatever repos are already on the volume, or none.
		//
		// It starts with control replication OFF, though. Opening the registry
		// after this creates an EMPTY control.db, and replicating that would
		// snapshot it over the registry this restore failed to read — turning a
		// transient failure into the permanent loss of every repo's name and
		// origin, which no re-clone reconstructs because they live nowhere else.
		log.Error().Err(err).Msg("backup: could not rehydrate control.db; starting with whatever is on " +
			"this volume, and NOT replicating control.db this boot so the registry in the replica survives — " +
			"restart once the replica is reachable to resume replicating it")
		return false
	}

	// Past the restore, so the control.db that ends up on disk is either the
	// replica's own copy or one the replica legitimately never had. Everything
	// below this line is about REPOS, whose failures are ordinary cache misses,
	// so control replication stays on however they go.
	replicateControl = true

	// Opening the registry runs migrate.Control, and CREATES control.db when it
	// is absent — which is precisely why it cannot happen before the restore
	// above. An empty control.db here means an empty intended set.
	reg, err := repos.OpenRepoRegistry(controlPath)
	if err != nil {
		log.Error().Err(err).Msg("backup: could not open the repo registry; skipping repo rehydration")
		return replicateControl
	}
	intended, err := reg.List(repos.RepoActive)
	if cerr := reg.Close(); cerr != nil {
		log.Warn().Err(cerr).Msg("closing registry after bootstrap read")
	}
	if err != nil {
		log.Error().Err(err).Msg("backup: could not read the intended repo set; skipping repo rehydration")
		return replicateControl
	}

	report := bm.RestoreRepos(ctx, intended)
	if len(report.Failed) > 0 {
		names := make([]string, 0, len(report.Failed))
		for name, ferr := range report.Failed {
			log.Error().Err(ferr).Str("repo", name).Msg("backup: restore failed; this repo will be rebuilt from its origin")
			names = append(names, name)
		}
		sort.Strings(names)
		// ERROR rather than a refused boot: the replica is a cache, and every
		// name here is re-clonable from the origin its registry row records. A
		// repo with NO origin is the one case this costs something, and
		// repos.Manager.Start says so by name when it reaches it.
		log.Error().Strs("repos", names).
			Msg("backup: some repos could not be rehydrated from the replica; starting anyway")
	}
	if len(report.NoSnapshot) > 0 {
		// Not a failure: this is how a first boot looks, and how a repo that
		// needs an origin clone looks. repos.Manager.Start reconciles them.
		log.Info().Strs("repos", report.NoSnapshot).
			Msg("no backup found for these repos; they will be rebuilt from origin")
	}
	return replicateControl
}
