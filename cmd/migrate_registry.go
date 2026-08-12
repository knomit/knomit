// `knomit migrate-registry`: the one-shot conversion of a pre-registry home to
// the control.db repo registry. It runs once, with the server stopped, and it
// is the ONLY code in the tree that knows the old on-disk shape.
//
// A legacy home looks like this:
//
//	<home>/repos/<name>.db              repo databases named by REPO NAME, whose
//	                                    `remotes` row still carries url, branch,
//	                                    auth_method and auth_token
//	<home>/repos/archive/<ksuid>.db     archived repos
//	<home>/repos/archive/<ksuid>.json   their manifests
//	<home>/control.db                   lenses/lens_reads in the NAME-keyed shape
//	                                    plus repo_settings(repo_id, profile);
//	                                    no repos and no repo_origins table
//
// and this tool leaves behind the shape the rest of the codebase now assumes:
// `repos` + `repo_origins` populated, every database file renamed to
// repos/<uid>.db, lens rows re-keyed to uids, repo_settings folded into
// repos.profile and dropped, the archive directory gone, and each repo database
// migrated to schema 000017.
//
// # THE ORDERING INVARIANT
//
// Migration 000017 DROPS remotes.url, .branch, .auth_method and .auth_token,
// and its own .down.sql says the values are not recoverable from the database
// afterwards. store.Open runs migrate.All. Therefore opening a legacy repo
// through the store to read its origin DESTROYS the origin before it can be
// read.
//
// Every phase of this tool that touches a legacy repo database before
// migrateRepoDatabases uses a RAW sql.Open handle. That includes the root-commit
// survey, which goes through storegit.NewStorer (no migrations) rather than
// store.Open. Every raw handle in the PLANNING phase is additionally read-only
// (openRaw, mode=ro); the one raw handle that writes is checkpointDatabase, and
// it runs in the write phase, after control.db has committed every captured
// origin. migrateRepoDatabases is the LAST step.
//
// Do not "simplify" any of the read paths below to store.Open. It would look
// tidier and it would silently destroy every user's stored credentials and
// remote URLs.
package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/segmentio/ksuid"
	"github.com/spf13/cobra"

	// Stock driver only: nothing this tool reads needs sqlite-vec, and the
	// custom "sqlite3_knomit" driver is registered lazily by store.Open, which
	// must not run until the very last phase.
	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	storegit "knomit/internal/store/git"
	migrate "knomit/internal/store/migrate"
)

// migrateOpts are the command's switches, split out so runMigrateRegistry is
// directly testable without cobra.
type migrateOpts struct {
	// DryRun prints the plan and returns before the first write. No file's
	// CONTENTS are created or modified — not control.db.bak, not a database —
	// because every read the plan makes goes through openRaw's mode=ro handle.
	// SQLite still writes its shared-memory bookkeeping: a -shm and a
	// zero-length -wal appear beside each database opened. That is measured,
	// not assumed (see snapshotTree in the test), and it is why this says
	// "contents" rather than "nothing".
	DryRun bool
	// Force overrides exactly one refusal: "the repos table already has rows".
	// It does NOT override the duplicate-identity abort, and it does not
	// override the already-migrated guard.
	Force bool
	// DropDanglingLensRefs turns an unresolvable lens member from an abort into
	// a reported drop.
	DropDanglingLensRefs bool
	// IgnoreRunningMarker proceeds despite <home>/running.marker. The marker is
	// also left behind by a crashed server, so a STALE one is common and needs
	// an override — but the override is explicit, because the alternative is
	// renaming database files out from under a live server's open handles.
	IgnoreRunningMarker bool
	// DiscardUnbackedOrigins confirms the one destructive side effect --force
	// has: DELETE FROM repos cascades to repo_origins, so a registered repo
	// whose database file is not on disk right now loses the stored url,
	// auth_method and encrypted token that repo_origins exists to keep
	// recoverable. --force alone refuses when that would happen.
	DiscardUnbackedOrigins bool
	// Out receives the plan/summary. nil means os.Stdout.
	Out io.Writer
}

func (o migrateOpts) out() io.Writer {
	if o.Out == nil {
		return os.Stdout
	}
	return o.Out
}

// migrateRegistryCmd builds the `knomit migrate-registry` subcommand.
func migrateRegistryCmd() *cobra.Command {
	var (
		home                 string
		dryRun               bool
		force                bool
		dropDanglingLensRefs bool
		ignoreRunningMarker  bool
		discardOrigins       bool
	)
	cmd := &cobra.Command{
		Use:   "migrate-registry",
		Short: "Convert a pre-registry knomit home to the control.db repo registry",
		Long: `Converts a home created before the repo registry moved into control.db.

Run it ONCE, with the server stopped. It reads every repo database's remote
connection config BEFORE migrating any of them (the schema migration that
follows drops those columns irrecoverably), records each repo in
control.db's repos/repo_origins tables, renames the database files to
repos/<uid>.db, re-keys lens membership from repo names to uids, folds
repo_settings into the registry row, and unpacks the archive directory.

Refuses to run while <home>/running.marker says a server may be up: renaming
databases out from under a live server is the one way this tool can corrupt
data.

This is IRREVERSIBLE. Only control.db is backed up (to control.db.bak);
each repo database's url/branch/auth_method/auth_token columns are dropped
by schema migration 000017 and are not recoverable from the database
afterwards, by that migration's own admission. COPY THE WHOLE HOME BEFORE
RUNNING.

--dry-run prints the plan and returns before the first write. It modifies
no file CONTENTS: every read goes through a mode=ro handle. SQLite still
creates a -shm and a zero-length -wal beside each database it opens;
neither carries committed data and the next clean read-write close clears
both.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if home == "" {
				cfg, err := config.Load()
				if err != nil {
					return fmt.Errorf("load config: %w", err)
				}
				home = cfg.Home
			}
			return runMigrateRegistry(home, migrateOpts{
				DryRun:                 dryRun,
				Force:                  force,
				DropDanglingLensRefs:   dropDanglingLensRefs,
				IgnoreRunningMarker:    ignoreRunningMarker,
				DiscardUnbackedOrigins: discardOrigins,
				Out:                    cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "knomit home directory (default: the configured home)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the plan and return before the first write (no file contents are changed)")
	cmd.Flags().BoolVar(&force, "force", false, "migrate even though the repos table already has rows (re-run)")
	cmd.Flags().BoolVar(&dropDanglingLensRefs, "drop-dangling-lens-refs", false,
		"drop lens references to repos that cannot be resolved instead of aborting")
	cmd.Flags().BoolVar(&ignoreRunningMarker, "ignore-running-marker", false,
		"proceed despite <home>/running.marker (only when you have confirmed no server is running)")
	cmd.Flags().BoolVar(&discardOrigins, "discard-unbacked-origins", false,
		"with --force: confirm discarding the stored remote config of registered repos whose database file is missing")
	return cmd
}

// ---------------------------------------------------------------------------
// captured legacy state
// ---------------------------------------------------------------------------

// legacyOrigin is one legacy `remotes` row's CONNECTION half — exactly the four
// columns migration 000017 drops. AuthTokenCipher is the stored ciphertext and
// is copied verbatim: this tool never holds an agent key and never decrypts a
// credential. store.NewCrypt derives its key from the agent key material alone
// with no per-repo salt, so the ciphertext stays readable wherever it lands.
type legacyOrigin struct {
	URL             string
	Branch          string
	AuthMethod      string
	AuthTokenCipher string
}

// repoPlan is one repo's full migration: what it is now, and what it becomes.
type repoPlan struct {
	Name       string
	UID        string
	Archived   bool
	SrcDB      string // where the database file is now
	DstDB      string // <home>/repos/<uid>.db
	Manifest   string // archive manifest to delete ("" for active repos)
	RootCommit string
	// RootCommitErr explains an EMPTY RootCommit: the repo's HEAD could not be
	// resolved, so it is registered with a NULL repo_id and skipped by the
	// duplicate-identity survey. Empty when the root commit resolved.
	RootCommitErr string
	Origin        *legacyOrigin // nil when the repo had no remote configured
	Profile       string
	CreatedAt     int64
	ArchivedAt    int64
}

// skippedFile is a .db in repos/ that is not a knomit repository. It is left
// exactly where it is: the tool neither registers nor deletes it.
type skippedFile struct {
	Path   string
	Reason string
}

// lensPlan is one lens, already translated to uids.
type lensPlan struct {
	// UID is the lens's own registry identity (lenses.uid) it will land with.
	// Carried across unchanged when the source row already has one (a re-run
	// against a control.db this same tool already re-keyed); minted fresh
	// otherwise.
	UID         string
	Name        string
	WriteUID    string
	Description string
	CreatedAt   int64
	UpdatedAt   int64
	Reads       []lensReadPlan
	// Dropped lists the legacy member references that could not be resolved and
	// were dropped under --drop-dangling-lens-refs.
	Dropped []string
}

type lensReadPlan struct {
	RepoUID string
	Branch  string
	Source  string
}

// migrationPlan is the whole conversion, computed entirely from read-only
// access before a single byte is written.
type migrationPlan struct {
	Home        string
	ControlPath string
	ReposDir    string
	ArchiveDir  string
	Repos       []repoPlan
	Lenses      []lensPlan
	// LensMembersKeyedByUID is true when control.db's lens tables were found in
	// the new shape (a re-run) — i.e. the lens MEMBER references (write_uid /
	// repo_uid) are already keyed by repo uid rather than repo name. Probed
	// from the write_uid column. This says nothing about whether the lens ROW
	// ITSELF has its own uid — that is the separate hasOwnUID probe (the
	// "uid" column), added later alongside OpenLensRegistry's self-upgrade;
	// a home can have this true and hasOwnUID false. When this is true, rows
	// are carried across unchanged.
	LensMembersKeyedByUID bool
	// ControlDBExists is false for a home that never had a control.db, in which
	// case there is nothing to back up.
	ControlDBExists bool
	// LegacyControlDB is true when control.db still carries pre-registry shape
	// that only this tool removes. It is what makes a home with NO repo
	// databases still worth converting — see the empty-home branch in
	// planMigration and detectLegacyControlDB.
	LegacyControlDB bool
	// ForceResetRows is true when an existing populated repos table is being
	// rebuilt under --force.
	ForceResetRows bool
	// SessionSidecars are ephemeral .sessions.db files to delete.
	SessionSidecars []string
	// DroppedLenses names lenses discarded whole because their WRITE repo did
	// not resolve (only reachable under --drop-dangling-lens-refs).
	DroppedLenses []string
	// Skipped lists .db files in repos/ that are not knomit repositories and
	// are left untouched.
	Skipped []skippedFile
	// UnbackedOrigins are registered repos that --force's DELETE FROM repos
	// would cascade the stored remote config away from: their database file is
	// not on disk, so the rebuild-from-disk scan will not re-register them.
	UnbackedOrigins []unbackedOrigin
}

// unbackedOrigin is a registry row whose stored connection --force would
// destroy. repo_origins exists so a lost .db can be re-cloned from the record
// that outlives it; these are exactly the rows where that promise is live.
type unbackedOrigin struct {
	UID  string
	Name string
	URL  string
}

// ---------------------------------------------------------------------------
// the tool
// ---------------------------------------------------------------------------

// runMigrateRegistry converts the home at the given path. It is safe to run
// against a home that is already partly converted only in the narrow sense
// documented on --force; against a fully migrated home it refuses.
func runMigrateRegistry(home string, opts migrateOpts) error {
	out := opts.out()

	reposDir := filepath.Join(home, "repos")
	if st, err := os.Stat(reposDir); err != nil || !st.IsDir() {
		return fmt.Errorf("no repos directory at %s: this does not look like a knomit home", reposDir)
	}
	if err := refuseIfServerRunning(home, opts); err != nil {
		return err
	}

	// ---- read-only phases ------------------------------------------------
	//
	// The whole plan — origin capture, root-commit survey, lens translation,
	// profile fold — is computed through openRaw, which opens mode=ro. So an
	// abort anywhere below leaves every data-bearing file in the home
	// unmodified, no control.db.bak among them, and no database silently
	// checkpointed. (SQLite still touches its shared-memory bookkeeping: the
	// -shm index is rewritten and an EMPTY -wal may appear. Neither carries
	// committed data and the next clean read-write close clears both. That is
	// measured, not assumed — see snapshotTree in the test.) The backup is
	// taken after the plan is known good and before the first write.
	plan, err := planMigration(home, opts)
	if err != nil {
		return err
	}

	printPlan(out, plan)

	if opts.DryRun {
		fmt.Fprintln(out, "\ndry run: no file contents were changed. "+
			"(SQLite may have left a -shm and an empty -wal beside each database it opened; "+
			"neither carries committed data.)")
		return nil
	}

	if len(plan.UnbackedOrigins) > 0 && !opts.DiscardUnbackedOrigins {
		names := make([]string, 0, len(plan.UnbackedOrigins))
		for _, o := range plan.UnbackedOrigins {
			names = append(names, fmt.Sprintf("%s (uid=%s) %s", o.Name, o.UID, o.URL))
		}
		return fmt.Errorf(
			"--force would discard the stored remote config of %d registered repo(s) whose "+
				"database file is not on disk:\n  %s\n"+
				"repo_origins is what makes a lost .db re-clonable, and the credential is stored "+
				"nowhere else. Put the missing database files back and re-run, or pass "+
				"--discard-unbacked-origins to confirm losing them",
			len(names), strings.Join(names, "\n  "))
	}

	// ---- writes ----------------------------------------------------------
	if plan.ControlDBExists {
		bak, berr := backupControlDB(plan.ControlPath)
		if berr != nil {
			return berr
		}
		fmt.Fprintf(out, "\nbacked up %s -> %s\n", plan.ControlPath, bak)
	}

	// Steps 4-7 in ONE transaction: registry rows, origins, the lens rebuild
	// and the repo_settings fold either all land or none do, so a failure
	// anywhere in there cannot leave a half-built registry behind.
	if err := applyControlDB(plan); err != nil {
		return err
	}
	fmt.Fprintln(out, "control.db written")

	// File renames cannot join that transaction, so they run after the commit.
	// A failure here leaves control.db correct and the files behind it, which
	// is recoverable BY HAND from the mapping printed on the way out.
	if err := moveRepoFiles(out, plan); err != nil {
		return err
	}

	// Only now: migrating a repo database drops the four connection columns,
	// and every one of them is already recorded in control.db above.
	if err := migrateRepoDatabases(out, plan); err != nil {
		return err
	}

	printSummary(out, plan)
	return nil
}

// refuseIfServerRunning refuses to convert a home whose server may still be up.
//
// This is the one scenario in which this tool can genuinely corrupt data. A
// POSIX rename succeeds silently against an open file descriptor, so
// moveRepoFiles would move every database out from under a live server's
// handles — which keep writing to the now-renamed inode — and then
// migrateRepoDatabases would run migrate.All on databases that server is
// concurrently writing. "Run it with the server stopped" is documentation;
// this is the check.
//
// serve.go (cmd/serve.go) writes <home>/running.marker at boot and clears it on
// a clean shutdown, so its presence means "a server is running, OR one died
// without cleaning up". Both are worth stopping for, and the second is why
// there is an override.
func refuseIfServerRunning(home string, opts migrateOpts) error {
	markerPath := filepath.Join(home, "running.marker")
	data, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", markerPath, err)
	}
	if opts.IgnoreRunningMarker {
		return nil
	}
	since := strings.TrimSpace(string(data))
	if since == "" {
		since = "unknown"
	}
	return fmt.Errorf(
		"%s exists (server started %s): a knomit server may be running on this home, and "+
			"migrating underneath one renames database files out from under its open handles.\n"+
			"Stop the server and re-run. If nothing is running (the marker is also left behind "+
			"by a crash), check with `pgrep -fl 'knomit serve'` and then pass "+
			"--ignore-running-marker",
		markerPath, since)
}

// ---------------------------------------------------------------------------
// planning (read-only)
// ---------------------------------------------------------------------------

func planMigration(home string, opts migrateOpts) (*migrationPlan, error) {
	plan := &migrationPlan{
		Home:        home,
		ControlPath: filepath.Join(home, "control.db"),
		ReposDir:    filepath.Join(home, "repos"),
		ArchiveDir:  filepath.Join(home, "repos", "archive"),
	}
	if _, err := os.Stat(plan.ControlPath); err == nil {
		plan.ControlDBExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", plan.ControlPath, err)
	}
	if plan.ControlDBExists {
		legacy, err := detectLegacyControlDB(plan.ControlPath)
		if err != nil {
			return nil, err
		}
		plan.LegacyControlDB = legacy
	}

	// Step 1a: refuse a home whose registry already has rows, unless --force.
	var existing existingRegistry
	if plan.ControlDBExists {
		var err error
		existing, err = readExistingRegistry(plan.ControlPath)
		if err != nil {
			return nil, err
		}
		if existing.Rows > 0 {
			if !opts.Force {
				return nil, fmt.Errorf(
					"%s already has %d row(s) in the repos table: this home looks migrated. "+
						"Re-run with --force only if you know the previous run did not finish",
					plan.ControlPath, existing.Rows)
			}
			plan.ForceResetRows = true
		}
	}

	// Step 2: capture. Read every legacy repo database — active AND archived —
	// with a raw handle, BEFORE anything migrates any of them.
	active, err := captureActiveRepos(plan, existing)
	if err != nil {
		return nil, err
	}
	archived, err := captureArchivedRepos(plan)
	if err != nil {
		return nil, err
	}
	plan.Repos = append(active, archived...)
	// A home with no repo databases is NOT automatically a home with nothing to
	// migrate. The old Manager.Start opened the lens registry unconditionally,
	// so every home that ever ran the old server carries `lenses.write_repo` —
	// including one that never held a repo, or that later purged them all. That
	// column is arm 1 of refuseUnmigratedHome, so such a home refuses to boot;
	// aborting here would leave the operator with a server that refuses forever
	// and a migration tool that says there is nothing to do. applyControlDB
	// converts a zero-repo home perfectly well: it drops and rebuilds the lens
	// tables, creates repos/repo_origins, and drops repo_settings.
	if len(plan.Repos) == 0 && !plan.LegacyControlDB {
		if len(plan.Skipped) > 0 {
			var paths []string
			for _, s := range plan.Skipped {
				paths = append(paths, s.Path)
			}
			return nil, fmt.Errorf(
				"no knomit repo databases found under %s: nothing to migrate. "+
					"These .db files are not knomit repositories and were ignored:\n  %s",
				plan.ReposDir, strings.Join(paths, "\n  "))
		}
		return nil, fmt.Errorf("no repo databases found under %s: nothing to migrate", plan.ReposDir)
	}

	// What --force would take with it. repo_origins cascades from repos, so
	// DELETE FROM repos drops the stored url/auth of every registered repo the
	// disk scan above did NOT find — precisely the repos that record exists to
	// make recoverable. Computed here so printPlan can show it and the caller
	// can refuse.
	if plan.ForceResetRows {
		planned := make(map[string]bool, len(plan.Repos))
		for _, rp := range plan.Repos {
			planned[rp.UID] = true
		}
		for uid, u := range existing.OriginURLByUID {
			if planned[uid] {
				continue
			}
			plan.UnbackedOrigins = append(plan.UnbackedOrigins, unbackedOrigin{
				UID: uid, Name: existing.NameByUID[uid], URL: u,
			})
		}
		sort.Slice(plan.UnbackedOrigins, func(i, j int) bool {
			return plan.UnbackedOrigins[i].Name < plan.UnbackedOrigins[j].Name
		})
	}

	// Step 3: duplicate-identity survey. The repos_active_repo_id constraint is
	// new, so an existing home may already violate it. No flag bypasses this.
	if err := surveyDuplicateIdentities(plan.Repos); err != nil {
		return nil, err
	}

	// Steps 6 and 7's inputs, still read-only: the legacy lens rows and the
	// repo_settings profiles.
	if err := planLenses(plan, opts); err != nil {
		return nil, err
	}
	if err := planProfiles(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// detectLegacyControlDB reports whether control.db still carries pre-registry
// shape — shape that `knomit migrate-registry` is the only thing that removes,
// and that Manager.Start's refuseUnmigratedHome guard reads as "this home is
// unconverted, do not boot".
//
// Three signals, in the order they cost: no `repos` table at all (arm 3 of the
// guard), a name-keyed `lenses.write_repo` column (arm 1, and the one every
// legacy home has because the old Start opened the lens registry
// unconditionally), and a leftover `repo_settings` tenant this tool folds into
// repos.profile.
//
// This is only consulted for a home whose repos/ directory yielded nothing. A
// home WITH repo databases is converted on that evidence alone.
func detectLegacyControlDB(controlPath string) (bool, error) {
	db, err := openRaw(controlPath)
	if err != nil {
		return false, err
	}
	defer db.Close()

	hasRepos, err := rawTableExists(db, "repos")
	if err != nil {
		return false, err
	}
	if !hasRepos {
		return true, nil
	}
	hasLenses, err := rawTableExists(db, "lenses")
	if err != nil {
		return false, err
	}
	if hasLenses {
		nameKeyed, err := rawColumnExists(db, "lenses", "write_repo")
		if err != nil {
			return false, err
		}
		if nameKeyed {
			return true, nil
		}
	}
	return rawTableExists(db, "repo_settings")
}

// existingRegistry is what a control.db that already has a repos table can tell
// us about a previous run.
type existingRegistry struct {
	// Rows is the total row count; zero when the table does not exist.
	Rows int
	// ActiveUIDByName lets a --force re-run reuse the uids the previous run
	// minted rather than orphaning the lens rows that already point at them.
	ActiveUIDByName map[string]string
	// TakenUIDs is every registered uid, in any state — used to recognise a
	// database file a previous run already finished renaming.
	TakenUIDs map[string]bool
	// NameByUID and OriginURLByUID describe what --force's DELETE FROM repos
	// would take with it. repo_origins has ON DELETE CASCADE.
	NameByUID      map[string]string
	OriginURLByUID map[string]string
}

func readExistingRegistry(controlPath string) (existingRegistry, error) {
	out := existingRegistry{
		ActiveUIDByName: map[string]string{},
		TakenUIDs:       map[string]bool{},
		NameByUID:       map[string]string{},
		OriginURLByUID:  map[string]string{},
	}
	db, err := openRaw(controlPath)
	if err != nil {
		return out, err
	}
	defer db.Close()

	ok, err := rawTableExists(db, "repos")
	if err != nil || !ok {
		return out, err
	}
	rows, err := db.Query(`SELECT name, uid, state FROM repos`)
	if err != nil {
		return out, fmt.Errorf("read repos rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, uid, state string
		if err := rows.Scan(&name, &uid, &state); err != nil {
			return out, fmt.Errorf("read repos rows: %w", err)
		}
		out.Rows++
		out.TakenUIDs[uid] = true
		out.NameByUID[uid] = name
		if state == string(repos.StateActive) {
			out.ActiveUIDByName[name] = uid
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	hasOrigins, err := rawTableExists(db, "repo_origins")
	if err != nil || !hasOrigins {
		return out, err
	}
	orows, err := db.Query(`SELECT repo_uid, url FROM repo_origins`)
	if err != nil {
		return out, fmt.Errorf("read repo_origins rows: %w", err)
	}
	defer orows.Close()
	for orows.Next() {
		var uid, u string
		if err := orows.Scan(&uid, &u); err != nil {
			return out, fmt.Errorf("read repo_origins rows: %w", err)
		}
		out.OriginURLByUID[uid] = u
	}
	return out, orows.Err()
}

// captureActiveRepos reads every <home>/repos/<name>.db. The repo's NAME is its
// filename — that is exactly what this tool exists to stop being true.
func captureActiveRepos(plan *migrationPlan, existing existingRegistry) ([]repoPlan, error) {
	entries, err := os.ReadDir(plan.ReposDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", plan.ReposDir, err)
	}
	var out []repoPlan
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		if strings.HasSuffix(e.Name(), store.SessionDBSuffix) {
			// Ephemeral runtime state; deleted rather than carried across.
			plan.SessionSidecars = append(plan.SessionSidecars,
				filepath.Join(plan.ReposDir, e.Name()))
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".db")
		src := filepath.Join(plan.ReposDir, e.Name())

		// A file already named after a registered uid is a repo a previous run
		// finished renaming. Its "name" here would be the uid, and migrating it
		// again would register a second repo called after a ksuid.
		//
		// The advice is deliberately NOT "restore control.db.bak and re-run".
		// That backup predates the repos table, so restoring it empties
		// TakenUIDs, this guard stops firing, and the already-renamed
		// repos/<uid>.db is rescanned as a fresh active repo WHOSE NAME IS THE
		// KSUID. Undoing the renames is the repair; the failing run printed the
		// mapping for exactly this.
		if existing.TakenUIDs[name] {
			return nil, fmt.Errorf(
				"%s is named after a uid already in the repos table: a previous run got at "+
					"least this far. Move the already-renamed files back to their original "+
					"names (the failing run printed the mapping) and re-run with --force. Do "+
					"NOT restore control.db.bak first: that would re-register this file under "+
					"its ksuid as a repo NAME", src)
		}

		rp := repoPlan{Name: name, SrcDB: src, Profile: repos.ProfileCode}
		if uid, ok := existing.ActiveUIDByName[name]; ok {
			rp.UID = uid
		} else {
			rp.UID = ksuid.New().String()
		}
		rp.DstDB = filepath.Join(plan.ReposDir, rp.UID+".db")

		if err := captureRepoDatabase(&rp); err != nil {
			if errors.Is(err, errNotARepoDatabase) {
				plan.Skipped = append(plan.Skipped, skippedFile{Path: src, Reason: err.Error()})
				continue
			}
			return nil, err
		}

		// AFTER the capture, so a file that is simply not a repo is skipped and
		// an already-migrated one gets the more specific "past schema 000017"
		// refusal. What is left is a genuinely legacy database whose filename is
		// a bare ksuid with no registry row — the wreckage of restoring
		// control.db.bak over a partly-renamed home. Registering it would mint a
		// repo permanently NAMED after a ksuid, and nothing downstream could
		// tell that from a deliberate choice.
		if _, kerr := ksuid.Parse(name); kerr == nil {
			return nil, fmt.Errorf(
				"%s is named after a ksuid but has no row in the repos table. That is what a "+
					"partly-renamed home looks like after control.db.bak was restored over the "+
					"registry. Move it back to its original repo name and re-run with --force; "+
					"if it really is a repo you want called %q, rename it to something that is "+
					"not a ksuid first",
				src, name)
		}
		out = append(out, rp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Slice(plan.Skipped, func(i, j int) bool { return plan.Skipped[i].Path < plan.Skipped[j].Path })
	return out, nil
}

// captureArchivedRepos reads every repos/archive/<ksuid>.json manifest and its
// database. The ksuid is REUSED as the registry uid, so the file it names does
// not have to move under a new identity and any reference to the archive id
// still resolves.
func captureArchivedRepos(plan *migrationPlan) ([]repoPlan, error) {
	entries, err := os.ReadDir(plan.ArchiveDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", plan.ArchiveDir, err)
	}
	var out []repoPlan
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		manifestPath := filepath.Join(plan.ArchiveDir, e.Name())
		data, rerr := os.ReadFile(manifestPath)
		if rerr != nil {
			return nil, fmt.Errorf("read manifest %s: %w", manifestPath, rerr)
		}
		var info struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Origin     string `json:"origin"`
			ArchivedAt string `json:"archivedAt"`
		}
		if jerr := json.Unmarshal(data, &info); jerr != nil {
			return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, jerr)
		}
		uid := info.ID
		if uid == "" {
			uid = strings.TrimSuffix(e.Name(), ".json")
		}
		src := filepath.Join(plan.ArchiveDir, uid+".db")
		if _, serr := os.Stat(src); serr != nil {
			return nil, fmt.Errorf(
				"archived repo %q (%s) has a manifest but no database at %s: %w",
				info.Name, uid, src, serr)
		}
		archivedAt := time.Now().UTC().Unix()
		if t, perr := time.Parse(time.RFC3339Nano, info.ArchivedAt); perr == nil {
			archivedAt = t.UTC().Unix()
		}
		rp := repoPlan{
			Name:       info.Name,
			UID:        uid,
			Archived:   true,
			SrcDB:      src,
			DstDB:      filepath.Join(plan.ReposDir, uid+".db"),
			Manifest:   manifestPath,
			Profile:    repos.ProfileCode,
			ArchivedAt: archivedAt,
			CreatedAt:  archivedAt,
		}
		if rp.Name == "" {
			rp.Name = uid
		}
		// Strict, unlike the active scan: a manifest is an ASSERTION that an
		// archived knowledge base lives at this path. If it turns out not to be
		// a repo database, that is a discrepancy the operator must see, not a
		// stray file to skip past.
		if cerr := captureRepoDatabase(&rp); cerr != nil {
			if errors.Is(cerr, errNotARepoDatabase) {
				return nil, fmt.Errorf(
					"archived repo %q (%s) has a manifest but %s is not a knomit repo database; "+
						"move the manifest aside if the archive is genuinely gone",
					rp.Name, uid, src)
			}
			return nil, cerr
		}
		if rp.CreatedAt == 0 {
			rp.CreatedAt = archivedAt
		}
		out = append(out, rp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })

	// The reverse discrepancy, and the more dangerous one: a database in the
	// archive with NO manifest. The loop above iterates manifests, so such a
	// file is invisible to the plan — it is neither registered nor moved, the
	// summary reports success, and moveRepoFiles then finds the directory
	// non-empty and leaves it behind while every surface says the archive is
	// gone. An archived knowledge base sitting in a directory nothing will look
	// in again. Mirror the strictness the manifest branch already applies in the
	// other direction and refuse.
	planned := make(map[string]bool, len(out))
	for _, rp := range out {
		planned[filepath.Base(rp.SrcDB)] = true
	}
	var orphans []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		if store.IsSessionDBFile(e.Name()) || planned[e.Name()] {
			continue
		}
		orphans = append(orphans, filepath.Join(plan.ArchiveDir, e.Name()))
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, fmt.Errorf(
			"these databases are in the archive directory with no manifest naming them, so "+
				"nothing can say what they are or which repo they were:\n  %s\n"+
				"Move them somewhere outside %s and re-run. They are not deleted and not "+
				"registered — this tool refuses to guess an identity for a knowledge base",
			strings.Join(orphans, "\n  "), plan.ReposDir)
	}
	return out, nil
}

// errNotARepoDatabase marks a .db file that is not a knomit repository at all —
// most often a zero-byte macOS duplication artifact ("core 1.db",
// "knomit-kb 3.db"). It opens fine as an empty SQLite database and simply has
// no tables.
//
// This is a SKIP, not a refusal. Getting the distinction wrong is worse than it
// sounds: the file has no `remotes.url` column, so lumping it in with a
// migrated database produces "this home is already migrated" — the one
// conclusion that stops an operator from trying again, stated about a home that
// has not been migrated at all.
var errNotARepoDatabase = errors.New("not a knomit repo database")

// captureRepoDatabase fills in a repo's origin, root commit and birth time from
// its legacy database. It returns errNotARepoDatabase (wrapped) for a file that
// is not a knomit repository; callers decide whether to skip or refuse.
//
// RAW HANDLES ONLY. See the ORDERING INVARIANT at the top of this file: going
// through store.Open here would run migration 000017 and destroy the very row
// being read.
func captureRepoDatabase(rp *repoPlan) error {
	db, err := openRaw(rp.SrcDB)
	if err != nil {
		return err
	}
	defer db.Close()

	// No `remotes` table at all: not a knomit repo database. Note that a
	// genuinely corrupt or non-SQLite file fails this query outright, and that
	// error is returned as a REFUSAL rather than a skip — a file that might be
	// somebody's knowledge base must never be silently left behind.
	hasRemotes, err := rawTableExists(db, "remotes")
	if err != nil {
		return fmt.Errorf("inspect %s: %w", rp.SrcDB, err)
	}
	if !hasRemotes {
		return fmt.Errorf("%s: %w (no remotes table)", rp.SrcDB, errNotARepoDatabase)
	}

	// A `remotes` table WITHOUT the connection columns is a real repo database
	// that has already been migrated, so this home is not the legacy shape this
	// tool converts. Refuse — with or without --force. Guessing here would
	// delete the repo_origins rows a previous run wrote and replace them with
	// nothing.
	hasURL, err := rawColumnExists(db, "remotes", "url")
	if err != nil {
		return fmt.Errorf("inspect %s: %w", rp.SrcDB, err)
	}
	if !hasURL {
		return fmt.Errorf(
			"%s has a remotes table with no url column, so it has already been migrated past "+
				"schema 000017: this home is not in the pre-registry shape and migrate-registry "+
				"must not run on it",
			rp.SrcDB)
	}

	origin, err := readLegacyOrigin(db)
	if err != nil {
		return fmt.Errorf("read origin from %s: %w", rp.SrcDB, err)
	}
	rp.Origin = origin

	// An unresolvable HEAD DEGRADES rather than aborting: one repo with a
	// broken or missing ref must not block converting the other nine. The repo
	// is registered with a NULL repo_id, which the registry explicitly allows
	// (it is filled in on the first successful open), it is excluded from the
	// duplicate-identity survey, and it cannot inherit a repo_settings profile
	// because that table is keyed by root commit. All three are reported.
	root, born, err := readRootCommit(db)
	if err != nil {
		rp.RootCommitErr = err.Error()
		if rp.CreatedAt == 0 {
			if st, serr := os.Stat(rp.SrcDB); serr == nil {
				rp.CreatedAt = st.ModTime().UTC().Unix()
			} else {
				rp.CreatedAt = time.Now().UTC().Unix()
			}
		}
		return nil
	}
	rp.RootCommit = root
	if rp.CreatedAt == 0 {
		rp.CreatedAt = born
	}
	return nil
}

// readLegacyOrigin returns the connection half of the repo's `remotes` row.
// The remote is conventionally named "origin"; any other single row is accepted
// so an oddly-named remote is carried across rather than silently dropped.
//
// auth_token is read and re-written as OPAQUE CIPHERTEXT. Nothing here decrypts.
func readLegacyOrigin(db *sql.DB) (*legacyOrigin, error) {
	// COALESCE even though all four columns are declared NOT NULL: this read
	// happens exactly once per installation and cannot be retried after the
	// migration has run, so a hand-edited row must not abort it.
	rows, err := db.Query(
		`SELECT name,
		        COALESCE(url, ''), COALESCE(branch, ''),
		        COALESCE(auth_method, ''), COALESCE(auth_token, '')
		   FROM remotes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found *legacyOrigin
	for rows.Next() {
		var name string
		var o legacyOrigin
		if err := rows.Scan(&name, &o.URL, &o.Branch, &o.AuthMethod, &o.AuthTokenCipher); err != nil {
			return nil, err
		}
		if o.URL == "" {
			continue // a status-only row: the repo has no remote configured
		}
		if name == "origin" {
			copied := o
			return &copied, rows.Err()
		}
		if found == nil {
			copied := o
			found = &copied
		}
	}
	return found, rows.Err()
}

// readRootCommit resolves the repo's identity — the root commit reached by a
// first-parent walk from HEAD — and the root commit's timestamp, which is the
// closest thing a legacy home has to the repo's creation time.
//
// It drives go-git over storegit.NewStorer, which wraps an existing *sql.DB and
// applies NO migrations. This is deliberate: it is the read that would
// otherwise tempt a caller into store.Open, which would drop the connection
// columns captured moments earlier.
func readRootCommit(db *sql.DB) (string, int64, error) {
	repo, err := gogit.Open(storegit.NewStorer(db), memfs.New())
	if err != nil {
		return "", 0, fmt.Errorf("git open: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return "", 0, fmt.Errorf("read HEAD: %w", err)
	}
	c, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", 0, fmt.Errorf("read head commit: %w", err)
	}
	for c.NumParents() > 0 {
		if c, err = c.Parent(0); err != nil {
			return "", 0, fmt.Errorf("walk parent: %w", err)
		}
	}
	return c.Hash.String(), c.Committer.When.UTC().Unix(), nil
}

// surveyDuplicateIdentities aborts when two ACTIVE repos hold the same
// knowledge base. repos_active_repo_id is a NEW constraint, so a home built
// before it may already violate it. Deliberately not overridable: two local
// copies of one knowledge base both write agent/<host> and clobber each other
// on push.
//
// The remediation it prints has to be one the operator can actually carry out.
// "Archive or purge one and re-run" was not: `knomit repo archive` talks to a
// SERVER, and the server refuses to boot on an unmigrated home — which is
// exactly the home this message appears on. So the advice is a file move, which
// needs nothing running, and the message gives absolute paths rather than
// names, and EVERY colliding pair rather than the first one found (fix one,
// re-run, meet the next: a loop nobody signed up for).
func surveyDuplicateIdentities(all []repoPlan) error {
	byRoot := map[string][]repoPlan{}
	var order []string
	for _, rp := range all {
		if rp.Archived || rp.RootCommit == "" {
			continue
		}
		if _, seen := byRoot[rp.RootCommit]; !seen {
			order = append(order, rp.RootCommit)
		}
		byRoot[rp.RootCommit] = append(byRoot[rp.RootCommit], rp)
	}
	var groups []string
	for _, root := range order {
		group := byRoot[root]
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Name < group[j].Name })
		lines := make([]string, 0, len(group)+1)
		lines = append(lines, fmt.Sprintf("knowledge base %s is held by %d repos:", shortID(root), len(group)))
		for _, rp := range group {
			lines = append(lines, fmt.Sprintf("    %-20s %s", rp.Name, rp.SrcDB))
		}
		groups = append(groups, strings.Join(lines, "\n  "))
	}
	if len(groups) == 0 {
		return nil
	}
	return fmt.Errorf(
		"two or more ACTIVE repos hold the same knowledge base, which the registry "+
			"does not allow (both copies write agent/<host> and clobber each other on push):\n  %s\n"+
			"Keep ONE of each group and move the other .db files (with any -wal/-shm) out of the "+
			"repos directory entirely, then re-run. Do not use `knomit repo archive`: it talks to a "+
			"server, and the server refuses to boot on an unmigrated home",
		strings.Join(groups, "\n  "))
}

func shortID(id string) string {
	if len(id) < 12 {
		return id
	}
	return id[:12]
}

// planLenses reads the legacy lens tables and translates every member reference
// from a repo NAME to a registry uid.
func planLenses(plan *migrationPlan, opts migrateOpts) error {
	if !plan.ControlDBExists {
		return nil
	}
	db, err := openRaw(plan.ControlPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ok, err := rawTableExists(db, "lenses")
	if err != nil || !ok {
		return err
	}
	// A control.db whose lenses table already has write_uid was built (or
	// repaired) by a previous run; its rows are carried across unchanged and
	// only checked for danglers.
	newShape, err := rawColumnExists(db, "lenses", "write_uid")
	if err != nil {
		return err
	}
	plan.LensMembersKeyedByUID = newShape
	writeCol, readCol := "write_repo", "repo"
	if newShape {
		writeCol, readCol = "write_uid", "repo_uid"
	}

	// A control.db this same tool already re-keyed (write_uid present) may or
	// may not already carry the lens's OWN uid too — that column was added
	// alongside OpenLensRegistry's self-upgrade, so an already-uid-keyed home
	// this tool has not yet touched under the new version does not have it
	// yet. Read it when present so a --force re-run does not mint a second,
	// different identity for a lens that already has one; mint fresh below
	// when it is absent.
	hasOwnUID := false
	if newShape {
		hasOwnUID, err = rawColumnExists(db, "lenses", "uid")
		if err != nil {
			return err
		}
	}

	// name -> uid for ACTIVE repos, with archived names as a fallback. The old
	// archive guard refused to archive a lens member, so a lens should only
	// ever name an active repo; the fallback covers a home where that guard was
	// bypassed out of band.
	byName := map[string]string{}
	for _, rp := range plan.Repos {
		if !rp.Archived {
			byName[rp.Name] = rp.UID
		}
	}
	knownUID := map[string]struct{}{}
	for _, rp := range plan.Repos {
		knownUID[rp.UID] = struct{}{}
		if _, taken := byName[rp.Name]; !taken {
			byName[rp.Name] = rp.UID
		}
	}
	resolve := func(ref string) (string, bool) {
		if newShape {
			_, ok := knownUID[ref]
			return ref, ok
		}
		uid, ok := byName[ref]
		return uid, ok
	}

	lensSelectCols := "name, " + writeCol + ", description, created_at, updated_at"
	if hasOwnUID {
		lensSelectCols = "uid, " + lensSelectCols
	}
	rows, err := db.Query(`SELECT ` + lensSelectCols + ` FROM lenses ORDER BY name`)
	if err != nil {
		return fmt.Errorf("read legacy lenses: %w", err)
	}
	var lenses []lensPlan
	type rawLens struct {
		uid, name, write, desc string
		created, updated       int64
	}
	var raws []rawLens
	for rows.Next() {
		var rl rawLens
		var scanErr error
		if hasOwnUID {
			scanErr = rows.Scan(&rl.uid, &rl.name, &rl.write, &rl.desc, &rl.created, &rl.updated)
		} else {
			scanErr = rows.Scan(&rl.name, &rl.write, &rl.desc, &rl.created, &rl.updated)
		}
		if scanErr != nil {
			rows.Close()
			return fmt.Errorf("read legacy lenses: %w", scanErr)
		}
		raws = append(raws, rl)
	}
	rerr := rows.Err()
	rows.Close()
	if rerr != nil {
		return fmt.Errorf("read legacy lenses: %w", rerr)
	}

	var dangling []string
	var droppedLenses []string
	readsExist, err := rawTableExists(db, "lens_reads")
	if err != nil {
		return err
	}
	for _, rl := range raws {
		uid := rl.uid
		if uid == "" {
			uid = ksuid.New().String()
		}
		lp := lensPlan{
			UID:         uid,
			Name:        rl.name,
			Description: rl.desc,
			CreatedAt:   rl.created,
			UpdatedAt:   rl.updated,
		}
		writeUID, wok := resolve(rl.write)
		if !wok {
			// A lens with no write repo is not a lens. Drop the whole row.
			dangling = append(dangling, fmt.Sprintf("lens %q write repo %q", rl.name, rl.write))
			droppedLenses = append(droppedLenses, rl.name)
			continue
		}
		lp.WriteUID = writeUID

		if readsExist {
			// lens_reads.lens_uid replaces .lens_name in the exact same DDL
			// change that adds lenses.uid, so the two probes share hasOwnUID:
			// when the lens's own uid is already there, so is its child's.
			readsKeyCol, readsKeyVal := "lens_name", rl.name
			if hasOwnUID {
				readsKeyCol, readsKeyVal = "lens_uid", rl.uid
			}
			rrows, qerr := db.Query(
				`SELECT `+readCol+`, branch, COALESCE(source, '') FROM lens_reads WHERE `+readsKeyCol+` = ? ORDER BY `+readCol,
				readsKeyVal)
			if qerr != nil {
				return fmt.Errorf("read legacy lens reads: %w", qerr)
			}
			for rrows.Next() {
				var ref, branch, source string
				if err := rrows.Scan(&ref, &branch, &source); err != nil {
					rrows.Close()
					return fmt.Errorf("read legacy lens reads: %w", err)
				}
				uid, rok := resolve(ref)
				if !rok {
					dangling = append(dangling, fmt.Sprintf("lens %q read mount %q", rl.name, ref))
					lp.Dropped = append(lp.Dropped, ref)
					continue
				}
				lp.Reads = append(lp.Reads, lensReadPlan{RepoUID: uid, Branch: branch, Source: source})
			}
			if err := rrows.Err(); err != nil {
				rrows.Close()
				return fmt.Errorf("read legacy lens reads: %w", err)
			}
			rrows.Close()
		}
		// The write repo is always a read mount (LensRegistry.normalize does the
		// same); mirror it here so the rebuilt tables match what the registry
		// would have written.
		hasWrite := false
		for _, r := range lp.Reads {
			if r.RepoUID == lp.WriteUID {
				hasWrite = true
				break
			}
		}
		if !hasWrite {
			lp.Reads = append(lp.Reads, lensReadPlan{RepoUID: lp.WriteUID})
		}
		sort.Slice(lp.Reads, func(i, j int) bool { return lp.Reads[i].RepoUID < lp.Reads[j].RepoUID })
		lenses = append(lenses, lp)
	}

	if len(dangling) > 0 && !opts.DropDanglingLensRefs {
		return fmt.Errorf(
			"lens membership references repos that do not resolve:\n  %s\n"+
				"re-run with --drop-dangling-lens-refs to drop them "+
				"(a lens whose WRITE repo dangles is dropped entirely)",
			strings.Join(dangling, "\n  "))
	}
	sort.Strings(droppedLenses)
	plan.DroppedLenses = droppedLenses
	plan.Lenses = lenses
	return nil
}

// planProfiles folds repo_settings(repo_id, profile) onto the registry rows by
// matching the root commit. Absent and unrecognised values read as ProfileCode,
// which is exactly how the old RepoSettings.Profile behaved.
func planProfiles(plan *migrationPlan) error {
	if !plan.ControlDBExists {
		return nil
	}
	db, err := openRaw(plan.ControlPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ok, err := rawTableExists(db, "repo_settings")
	if err != nil || !ok {
		return err
	}
	rows, err := db.Query(`SELECT repo_id, profile FROM repo_settings`)
	if err != nil {
		return fmt.Errorf("read repo_settings: %w", err)
	}
	defer rows.Close()
	byRepoID := map[string]string{}
	for rows.Next() {
		var repoID, profile string
		if err := rows.Scan(&repoID, &profile); err != nil {
			return fmt.Errorf("read repo_settings: %w", err)
		}
		switch profile {
		case repos.ProfileCode, repos.ProfileChat, repos.ProfileGeneric:
			byRepoID[repoID] = profile
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read repo_settings: %w", err)
	}
	for i := range plan.Repos {
		// An unresolvable HEAD leaves RootCommit empty, and repo_settings is
		// keyed by root commit. Without this guard a legacy row with
		// repo_id='' would be applied to EVERY such repo — silently, and
		// contradicting what printPlan promises about them two screens up. The
		// old writer rejected an empty id so no shipped home has such a row;
		// this is two lines in a one-shot irreversible tool.
		if plan.Repos[i].RootCommit == "" {
			continue
		}
		if p, hit := byRepoID[plan.Repos[i].RootCommit]; hit {
			plan.Repos[i].Profile = p
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// writes
// ---------------------------------------------------------------------------

// backupControlDB checkpoints the WAL back into the main file and copies it
// aside. An existing backup is NEVER overwritten — it is the pristine
// pre-migration copy, and a --force re-run must not destroy it — so a second
// run writes control.db.bak.<unix> instead.
func backupControlDB(controlPath string) (string, error) {
	dst := controlPath + ".bak"
	if _, serr := os.Stat(dst); serr == nil {
		dst = fmt.Sprintf("%s.bak.%d", controlPath, time.Now().Unix())
	}

	// VACUUM INTO, NOT "checkpoint the WAL and copy the main file".
	//
	// The copy approach was silently unsound. `PRAGMA wal_checkpoint` reports
	// its outcome in a RESULT ROW (busy, log, checkpointed), not in an error:
	// with any other connection holding a read lock — a server the running
	// marker heuristic missed, a run forced through with
	// --ignore-running-marker, an editor with the file open — it returns
	// busy=1 having moved almost nothing, db.Exec discards that row, and the
	// subsequent os.ReadFile copies a main file whose committed rows are all
	// still in the -wal. Measured: busy=1 log=53 checkpointed=3, err=nil, and a
	// structurally valid backup containing 0 of 51 rows. The tool then printed
	// "backed up ..." and ran the irreversible migration on the strength of it.
	//
	// VACUUM INTO takes a read transaction and writes a fresh, fully consistent
	// database containing everything committed at that instant, WAL or no WAL,
	// concurrent readers or none. It errors loudly when it cannot. Requires
	// SQLite >= 3.27; the bundled library is far past that.
	//
	// NOT openRaw: VACUUM INTO is a write statement as far as _query_only=1 is
	// concerned (it writes the destination), so this open must be read-write.
	// It happens after the plan is known good, so it is already past the point
	// where "the home is untouched" is a promise this tool makes.
	db, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: controlPath}).String()+"?_busy_timeout=5000")
	if err != nil {
		return "", fmt.Errorf("open %s: %w", controlPath, err)
	}
	db.SetMaxOpenConns(1)
	_, verr := db.Exec(`VACUUM INTO ?`, dst)
	closeErr := db.Close()
	if verr != nil {
		return "", fmt.Errorf("back up %s to %s: %w", controlPath, dst, verr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", controlPath, closeErr)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return "", fmt.Errorf("chmod %s: %w", dst, err)
	}
	return dst, nil
}

// applyControlDB creates the control.db schema, then performs steps 4-7 in ONE
// transaction: insert every repo and its origin, rewrite the lens rows in the
// uid shape, and drop repo_settings once its profiles have been folded in.
//
// The schema step is deliberately OUTSIDE that transaction: golang-migrate
// opens its own transactions and cannot nest inside a caller's. So an abort
// now leaves empty-but-correct tables rather than no tables — a state
// indistinguishable from a fresh home, and one control.db.bak still rolls
// back in full. The row inserts stay atomic, which is the half that matters:
// a partially-populated registry is far worse than an empty one.
func applyControlDB(plan *migrationPlan) error {
	db, err := sql.Open("sqlite3",
		plan.ControlPath+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("open %s: %w", plan.ControlPath, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// The legacy lens tables go first: their rows are already captured in the
	// plan, they hold the OLD columns the baseline's IF NOT EXISTS would leave
	// in place, and lens_reads' foreign key into repos(uid) would otherwise
	// block the row rewrite below. lens_reads before lenses so the child is
	// gone before its parent.
	//
	// schema_migrations goes with them, and that is load-bearing. A home can
	// arrive here ALREADY stamped v1 with its lens tables still in the legacy
	// shape — controlUp skips the re-key for a `lenses.write_repo` home but
	// still runs the baseline, so any prior OpenRegistry touch stamps it.
	// Dropping the two tables and then calling a migrator that believes v1 is
	// applied would leave lenses and lens_reads simply GONE. Clearing the
	// stamp makes the baseline re-run; it is idempotent (IF NOT EXISTS
	// throughout), so repos and repo_origins and their rows are untouched.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS lens_reads`,
		`DROP TABLE IF EXISTS lenses`,
		`DROP TABLE IF EXISTS schema_migrations`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("drop legacy lens tables: %w", err)
		}
	}

	// Build every tenant from the versioned baseline. This is what the three
	// exported *SchemaSQL constants used to approximate; the .sql file is a
	// stricter guarantee against drift than a hand-copied constant.
	if err := migrate.Control(db); err != nil {
		return fmt.Errorf("create control.db schema: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin control.db transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if plan.ForceResetRows {
		// --force means "rebuild the registry from what is on disk". Origins
		// cascade from repos.
		if _, err := tx.Exec(`DELETE FROM repos`); err != nil {
			return fmt.Errorf("reset repos table: %w", err)
		}
	}

	for _, rp := range plan.Repos {
		state := string(repos.StateActive)
		var archivedAt any
		if rp.Archived {
			state = string(repos.StateArchived)
			archivedAt = rp.ArchivedAt
		}
		var repoID any
		if rp.RootCommit != "" {
			repoID = rp.RootCommit
		}
		if _, err := tx.Exec(
			`INSERT INTO repos (uid, name, state, profile, repo_id, created_at, archived_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rp.UID, rp.Name, state, rp.Profile, repoID, rp.CreatedAt, archivedAt,
		); err != nil {
			return fmt.Errorf("register repo %q: %w", rp.Name, err)
		}
		if rp.Origin == nil {
			continue
		}
		branch := rp.Origin.Branch
		if branch == "" {
			branch = "main"
		}
		// auth_token goes across as the ciphertext that was read. Nothing in
		// this tool decrypts a credential.
		if _, err := tx.Exec(
			`INSERT INTO repo_origins (repo_uid, url, branch, auth_method, auth_token)
			 VALUES (?, ?, ?, ?, ?)`,
			rp.UID, rp.Origin.URL, branch, rp.Origin.AuthMethod, rp.Origin.AuthTokenCipher,
		); err != nil {
			return fmt.Errorf("record origin for repo %q: %w", rp.Name, err)
		}
	}

	// Carry the translated lens rows across into the uid shape. This is the
	// step OpenLensRegistry cannot do for itself.
	for _, lp := range plan.Lenses {
		if _, err := tx.Exec(
			`INSERT INTO lenses (uid, name, write_uid, description, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			lp.UID, lp.Name, lp.WriteUID, lp.Description, lp.CreatedAt, lp.UpdatedAt,
		); err != nil {
			return fmt.Errorf("rewrite lens %q: %w", lp.Name, err)
		}
		for _, r := range lp.Reads {
			var source any
			if r.Source != "" {
				source = r.Source
			}
			if _, err := tx.Exec(
				`INSERT INTO lens_reads (lens_uid, repo_uid, branch, source) VALUES (?, ?, ?, ?)`,
				lp.UID, r.RepoUID, r.Branch, source,
			); err != nil {
				return fmt.Errorf("rewrite lens %q read mount: %w", lp.Name, err)
			}
		}
	}

	// The profiles are already on the repos rows above; the tenant that held
	// them is now dead weight.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS repo_settings`); err != nil {
		return fmt.Errorf("drop repo_settings: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit control.db transaction: %w", err)
	}
	return nil
}

// moveRepoFiles renames every database (and its -wal/-shm companions) to
// repos/<uid>.db, deletes the ephemeral session sidecars and the archive
// manifests, and removes the archive directory.
//
// These cannot join the control.db transaction. control.db is already correct
// by the time this runs, so a failure here is repaired by finishing the listed
// renames by hand — which is why the mapping is printed rather than merely
// returned.
// The archive manifest is the ONLY thing that makes an archived knowledge base
// discoverable while its database still sits in repos/archive/: captureArchivedRepos
// iterates .json files, and the server reads the registry rather than the
// directory. So a manifest may be removed ONLY once its database has actually
// arrived at repos/<uid>.db. Removing it after a failed rename does not delete
// the knowledge base — it makes it unreachable by every tool that could still
// find it, which is worse, because nothing reports it.
//
// Renaming therefore stops at the FIRST failure rather than plowing on: the
// files that did move are recorded in control.db, the ones that did not are
// printed, and the operator finishes a short list instead of auditing a long one.
func moveRepoFiles(out io.Writer, plan *migrationPlan) error {
	moved := make([]bool, len(plan.Repos))
	var failures []string

	for i := range plan.Repos {
		rp := plan.Repos[i]
		if rp.SrcDB == rp.DstDB {
			moved[i] = true
			continue
		}
		// Fold any uncheckpointed -wal back into the .db BEFORE the rename.
		// openRaw is read-only by design, so a crashed server's -wal has
		// travelled this whole run as a separate file holding committed
		// transactions; if the .db rename then succeeded and the -wal rename
		// failed, every one of those transactions would be silently rolled back
		// the next time anything opened the new path.
		if err := checkpointDatabase(rp.SrcDB); err != nil {
			failures = append(failures, err.Error())
			break
		}
		if err := renameDatabaseSet(rp.SrcDB, rp.DstDB); err != nil {
			failures = append(failures, err.Error())
			break // stop here: see the note above
		}
		moved[i] = true
		// The session sidecar is disposable runtime state, not knowledge.
		sess := store.SessionDBPathFor(rp.SrcDB)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(sess + suffix)
		}
	}
	for _, sidecar := range plan.SessionSidecars {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(sidecar + suffix)
		}
	}
	// Manifests: only for repos whose database actually reached its new home.
	for i := range plan.Repos {
		rp := plan.Repos[i]
		if rp.Manifest == "" || !moved[i] {
			continue
		}
		if err := os.Remove(rp.Manifest); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Sprintf("remove %s: %v", rp.Manifest, err))
		}
	}

	if len(failures) > 0 {
		fmt.Fprintln(out, "\ncontrol.db is already migrated; finish these renames by hand:")
		for i := range plan.Repos {
			if moved[i] {
				continue
			}
			rp := plan.Repos[i]
			fmt.Fprintf(out, "  %-24s uid=%s  %s -> %s\n", rp.Name, rp.UID, rp.SrcDB, rp.DstDB)
		}
		fmt.Fprintln(out, "  (repos not listed here have already moved and need nothing.)")
		return fmt.Errorf("moving repo database files failed:\n  %s", strings.Join(failures, "\n  "))
	}

	// Only after every archived database has moved out of it.
	if err := os.Remove(plan.ArchiveDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		if !isDirNotEmpty(err) {
			return fmt.Errorf("remove %s: %w", plan.ArchiveDir, err)
		}
		reportArchiveLeftovers(out, plan.ArchiveDir)
	}
	return nil
}

// checkpointDatabase folds a database's WAL back into the main file.
//
// The result of `PRAGMA wal_checkpoint` is a ROW — (busy, log, checkpointed) —
// not an error. db.Exec discards it, which is how the control.db backup came to
// be silently empty (see backupControlDB). Scan it and insist the checkpoint
// actually completed: busy must be 0 and every frame in the log must have been
// copied. A journal-mode database with no WAL answers (0, -1, -1), which
// satisfies both conditions and is correct — there was nothing to move.
//
// This is a READ-WRITE open, unlike every read in the planning phase. It is
// safe with respect to the ordering invariant at the top of this file because
// it uses a RAW handle: no migration runs, so the connection columns are not
// touched. It also runs in the write phase, after control.db has already
// committed every captured origin.
func checkpointDatabase(path string) error {
	db, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: path}).String()+"?_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("open %s to checkpoint: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	var busy, logFrames, checkpointed int
	scanErr := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
	closeErr := db.Close()
	if scanErr != nil {
		return fmt.Errorf("checkpoint %s: %w", path, scanErr)
	}
	if busy != 0 || checkpointed != logFrames {
		return fmt.Errorf(
			"checkpoint %s: incomplete (busy=%d log=%d checkpointed=%d); another process is "+
				"holding this database open — stop it and re-run with --force",
			path, busy, logFrames, checkpointed)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s after checkpoint: %w", path, closeErr)
	}
	return nil
}

// renameDatabaseSet moves a SQLite database and its -wal/-shm companions. A
// half-moved set is reported as one failure: the -wal of a crashed server holds
// committed transactions, and losing it silently rolls them back the next time
// anything opens the .db.
func renameDatabaseSet(srcDB, dstDB string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src, dst := srcDB+suffix, dstDB+suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("%s -> %s: %v", src, dst, err)
		}
	}
	return nil
}

// isDirNotEmpty reports whether err is "directory not empty" — the archive dir
// holding something this tool does not recognise. Leaving it in place with a
// warning beats deleting a file nobody planned for.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, os.ErrExist) || strings.Contains(err.Error(), "not empty")
}

// reportArchiveLeftovers makes good on isDirNotEmpty's promise of "a warning".
// Without it the summary claims the archive directory is gone while it is still
// there holding files, and the operator has no idea what they are.
func reportArchiveLeftovers(out io.Writer, archiveDir string) {
	fmt.Fprintf(out, "\n%s was NOT removed: it still contains files this tool did not plan for:\n", archiveDir)
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		fmt.Fprintf(out, "  (could not list it: %v)\n", err)
		return
	}
	for _, e := range entries {
		fmt.Fprintf(out, "  %s\n", filepath.Join(archiveDir, e.Name()))
	}
	fmt.Fprintln(out, "  Nothing was deleted. Inspect them and remove the directory by hand.")
}

// migrateRepoDatabases runs the schema migrations on every repo database, which
// is what finally drops remotes.url/.branch/.auth_method/.auth_token.
//
// THIS IS THE ONLY PLACE IN THIS TOOL THAT MAY CALL store.Open, and it runs
// last on purpose: every one of those four columns has already been read and
// committed to control.db's repo_origins table by applyControlDB above.
func migrateRepoDatabases(out io.Writer, plan *migrationPlan) error {
	for _, rp := range plan.Repos {
		svc, err := store.Open(rp.DstDB)
		if err != nil {
			return fmt.Errorf("migrate repo database %s (%s): %w", rp.Name, rp.DstDB, err)
		}
		cerr := svc.Close()
		if cerr != nil {
			return fmt.Errorf("close repo database %s (%s): %w", rp.Name, rp.DstDB, cerr)
		}
	}
	fmt.Fprintf(out, "migrated %d repo database(s) to the current schema\n", len(plan.Repos))
	return nil
}

// ---------------------------------------------------------------------------
// reporting
// ---------------------------------------------------------------------------

func printPlan(out io.Writer, plan *migrationPlan) {
	fmt.Fprintf(out, "home: %s\n", plan.Home)
	if plan.ForceResetRows {
		fmt.Fprintln(out, "--force: the existing repos rows will be rebuilt from what is on disk")
	}
	if plan.LensMembersKeyedByUID {
		fmt.Fprintln(out, "lens tables are already uid-keyed; their rows are carried across unchanged")
	}
	if len(plan.Repos) == 0 {
		fmt.Fprintln(out, "\nrepos: none on disk — this run converts control.db only "+
			"(rebuild the lens tables in the uid shape, create the empty registry,")
		fmt.Fprintln(out, "       drop repo_settings) so the server stops refusing to boot.")
	} else {
		fmt.Fprintln(out, "\nrepos:")
	}
	for _, rp := range plan.Repos {
		state := "active"
		if rp.Archived {
			state = "archived"
		}
		origin := "-"
		if rp.Origin != nil {
			origin = rp.Origin.URL
			if rp.Origin.AuthTokenCipher != "" {
				origin += " (credential carried over, never decrypted)"
			}
		}
		id := shortID(rp.RootCommit)
		if id == "" {
			id = "UNRESOLVED"
		}
		fmt.Fprintf(out, "  %-20s %-8s uid=%s id=%s profile=%s\n",
			rp.Name, state, rp.UID, id, rp.Profile)
		fmt.Fprintf(out, "      %s -> %s\n", rp.SrcDB, rp.DstDB)
		fmt.Fprintf(out, "      origin: %s\n", origin)
		if rp.RootCommitErr != "" {
			fmt.Fprintf(out, "      WARNING: HEAD unresolvable (%s)\n", rp.RootCommitErr)
			fmt.Fprintln(out, "               registered with no repo_id; it will be recorded on the first successful open,")
			fmt.Fprintln(out, "               it is excluded from the duplicate-identity check, and it cannot inherit a")
			fmt.Fprintln(out, "               repo_settings profile (that table is keyed by root commit)")
		}
	}
	if len(plan.UnbackedOrigins) > 0 {
		fmt.Fprintln(out, "\n--force WILL DISCARD the stored remote config of these registered repos")
		fmt.Fprintln(out, "(their database files are not on disk, so nothing re-registers them; repo_origins")
		fmt.Fprintln(out, "cascades from repos, and the encrypted credential exists nowhere else):")
		for _, o := range plan.UnbackedOrigins {
			fmt.Fprintf(out, "  %-20s uid=%s  %s\n", o.Name, o.UID, o.URL)
		}
	}
	if len(plan.Skipped) > 0 {
		fmt.Fprintln(out, "\nignored (not knomit repo databases; left exactly where they are):")
		for _, s := range plan.Skipped {
			fmt.Fprintf(out, "  %s\n", s.Path)
		}
	}
	if len(plan.Lenses) > 0 {
		fmt.Fprintln(out, "\nlenses (membership re-keyed to uids):")
		for _, lp := range plan.Lenses {
			fmt.Fprintf(out, "  %-20s write=%s reads=%d\n", lp.Name, lp.WriteUID, len(lp.Reads))
			for _, d := range lp.Dropped {
				fmt.Fprintf(out, "      dropped dangling member %q\n", d)
			}
		}
	}
	for _, name := range plan.DroppedLenses {
		fmt.Fprintf(out, "  dropped lens %q entirely: its write repo did not resolve\n", name)
	}
}

func printSummary(out io.Writer, plan *migrationPlan) {
	var active, archived int
	for _, rp := range plan.Repos {
		if rp.Archived {
			archived++
		} else {
			active++
		}
	}
	fmt.Fprintf(out,
		"\nmigrated: %d active repo(s), %d archived, %d lens(es)\n",
		active, archived, len(plan.Lenses))
	for _, rp := range plan.Repos {
		if rp.RootCommitErr != "" {
			fmt.Fprintf(out, "  %s registered without a repo_id: HEAD unresolvable (%s)\n",
				rp.Name, rp.RootCommitErr)
		}
	}
	for _, s := range plan.Skipped {
		fmt.Fprintf(out, "  ignored %s: %s\n", s.Path, s.Reason)
	}
	fmt.Fprintln(out, "start the server to verify; the backup printed above is the pre-migration copy.")
}

// ---------------------------------------------------------------------------
// raw SQLite helpers
// ---------------------------------------------------------------------------

// openRaw opens a SQLite file READ-ONLY with the STOCK driver and no
// migrations. Every planning-phase read in this tool goes through here.
//
// mode=ro is what makes "the plan writes nothing" a property SQLite enforces
// rather than one this file merely promises: without it, closing the last
// connection to a WAL database checkpoints the -wal back into the main file and
// unlinks -wal/-shm. That is content-preserving, but it is not nothing, and on
// an aborted run the home should be exactly as it was found. _query_only=1 is
// belt-and-braces: it rejects a write statement with a clear error instead of
// an obscure readonly-database one.
//
// mode=ro reads a WAL database left behind by a crashed server — uncommitted
// -wal content included — as long as the directory is writable, which it is
// (the tool is about to rename files in it). Verified against a database copied
// out mid-write with its -wal and -shm.
func openRaw(path string) (*sql.DB, error) {
	// url.URL escapes exactly what a file: URI needs and nothing more, so a
	// home containing spaces (macOS "core 1.db") resolves correctly.
	dsn := "file:" + (&url.URL{Path: path}).String() + "?mode=ro&_busy_timeout=5000&_query_only=1"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func rawTableExists(db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("look for table %q: %w", name, err)
	}
	return n > 0, nil
}

func rawColumnExists(db *sql.DB, table, column string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return false, fmt.Errorf("look for %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}
