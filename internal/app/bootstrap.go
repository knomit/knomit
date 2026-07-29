package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
}

// Bootstrap prepares KNOMIT_HOME before any database is opened.
//
// The ordering below is load-bearing, not stylistic. Restore refuses to
// overwrite a file that EXISTS, so anything that opens or creates a database
// before its restore has run turns that restore into a silent no-op — and the
// instance comes up with empty state that replication then writes over the good
// backup. Every step therefore has to complete before the next one can create a
// file the next one would have restored:
//
//  1. identity   — resolve agent_name + key. First because a backup-enabled
//     instance with an unstable identity must fail before it touches
//     the replica at all: it would otherwise restore one branch's
//     history and start writing to another.
//  2. backup     — build the replica client and PROBE the target, so bad
//     credentials fail the boot here rather than surfacing later as a
//     silent replication stall.
//  3. control.db — preflight, then restore. Must precede step 4: opening the
//     registry CREATES control.db when it is absent, which would
//     leave restore nothing to fill.
//  4. registry   — open control.db (this also runs migrate.Control) and read
//     the intended repo set. Must precede step 5: the registry is
//     the only record of which repo databases should exist — a
//     restored machine's repos/ directory is empty.
//  5. repos      — restore every intended database that is absent locally, then
//     preflight only the ones restore did NOT create.
//
// Steps 3-5 all finish before app.New runs repos.Manager.Start, which is what
// opens the repo databases for real.
func Bootstrap(ctx context.Context, cfg config.Config) (*BootResult, error) {
	// 1. Identity.
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
	res := &BootResult{Signer: signer, AgentBranch: agentBranch(cfg.AgentName, keyFingerprint)}
	log.Info().Str("agent_branch", res.AgentBranch).Msg("resolved agent identity")

	// 2. Backup client.
	bm, err := backup.Open(cfg.Backup, cfg.Home)
	if err != nil {
		return nil, err
	}
	if bm == nil {
		return res, nil // replication disabled: nothing to restore, nothing to guard
	}
	res.Backup = bm

	if err := restoreHome(ctx, cfg, bm); err != nil {
		// The manager never reaches the caller on this path, so nothing else
		// will ever close it — and its store already has compaction and
		// retention monitors running. Close it here rather than leaking them
		// past a refused boot.
		if cerr := bm.Close(context.Background()); cerr != nil {
			log.Warn().Err(cerr).Msg("closing backup manager after a refused boot")
		}
		return nil, err
	}
	return res, nil
}

// restoreHome runs steps 3-5: rehydrate control.db, read the intended repo set
// from it, and rehydrate every intended repo database that is absent.
func restoreHome(ctx context.Context, cfg config.Config, bm *backup.Manager) error {
	controlPath := filepath.Join(cfg.Home, "control.db")

	// Preflight BEFORE restoring, not after. The two are disjoint by
	// construction — Preflight is a no-op for an absent file and restore is a
	// no-op for a present one — so a control.db reaching Preflight here is
	// always one that restore will leave alone. Doing it in this order also
	// means a divergence refusal happens before anything has been written onto
	// the volume being refused.
	if err := bm.Preflight(ctx, "control", controlPath); err != nil {
		return err
	}
	if err := bm.RestoreControl(ctx); err != nil {
		return err
	}

	// Opening the registry runs migrate.Control, and CREATES control.db when it
	// is absent — which is precisely why it cannot happen before the restore
	// above. An empty control.db here means an empty intended set, i.e. every
	// repo backup silently orphaned.
	reg, err := repos.OpenRepoRegistry(controlPath)
	if err != nil {
		return fmt.Errorf("open repo registry: %w", err)
	}
	intended, err := reg.List(repos.RepoActive)
	if cerr := reg.Close(); cerr != nil {
		log.Warn().Err(cerr).Msg("closing registry after bootstrap read")
	}
	if err != nil {
		return fmt.Errorf("list intended repos: %w", err)
	}

	report, err := bm.RestoreRepos(ctx, intended)
	if err != nil {
		return err
	}
	if len(report.Failed) > 0 {
		for name, ferr := range report.Failed {
			log.Error().Err(ferr).Str("repo", name).Msg("restore failed")
		}
		return fmt.Errorf("restore failed for %d repo(s); refusing to start so empty state is not replicated over the backup", len(report.Failed))
	}
	if len(report.NoSnapshot) > 0 {
		// Not a failure: this is how a first boot looks, and how a repo that
		// needs an origin clone looks. repos.Manager.Start reconciles them.
		log.Info().Strs("repos", report.NoSnapshot).
			Msg("no backup found for these repos; they will be rebuilt from origin or refused by StrictMissing")
	}

	for _, name := range preflightTargets(intended, report.Restored) {
		if err := bm.Preflight(ctx, name, filepath.Join(cfg.Home, "repos", name+".db")); err != nil {
			return err
		}
	}
	return nil
}

// preflightTargets returns the intended repo names that must be preflighted:
// every name EXCEPT the ones restore just created.
//
// Preflight refuses the boot when the replica's transaction ID is ahead of the
// local database's, because that means two writers or a stale volume. A
// FRESHLY RESTORED database trips exactly that shape for an innocent reason:
// restore writes only the .db file, so there is no litestream shadow directory
// beside it and its local TXID reads 0 while the replica sits at N.
//
// Preflight cannot tell the two apart — at the file level they are identical —
// but Bootstrap can, because it knows something Preflight does not: this file
// did not exist a moment ago, so it cannot be a volume carrying stale content.
// Excluding restored names is therefore the narrow, correct rule, and it is
// what keeps Preflight's own "local TXID 0 is benign" allowance from having to
// cover the ordinary post-restore boot.
func preflightTargets(intended []repos.RepoRecord, restored []string) []string {
	skip := make(map[string]struct{}, len(restored))
	for _, name := range restored {
		skip[name] = struct{}{}
	}
	out := make([]string, 0, len(intended))
	for _, rec := range intended {
		if _, ok := skip[rec.Name]; ok {
			continue
		}
		out = append(out, rec.Name)
	}
	return out
}
