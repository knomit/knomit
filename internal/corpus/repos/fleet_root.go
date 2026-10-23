package repos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
	"knomit/internal/platform/privdir"
)

// Fleet root switch (master): export, install and stand down. StandDown takes
// consensusMerger.runMu, the one lock shared across clusters (sync and
// consensus), so it is what a future sync package would have to reach. See
// fleet_membership.go for the operations overview.

// FleetRoot is the master's root directory as GET /fleet/root returns it and
// PUT /fleet/root takes it: file name -> contents, for the names in
// fleetRootFiles only.
type FleetRoot struct {
	Dir             string            `json:"dir"`
	RootFingerprint string            `json:"root_fingerprint,omitempty"`
	Files           map[string]string `json:"files"`
}

// fleetRootFiles is every file a master's <home>/fleet holds: the key, its
// certificate, the CRL and the two local audit logs.
var fleetRootFiles = []string{pki.RootKeyFile, pki.RootCertFile, pki.CRLFile, pki.IssuedLogFile, pki.RevokedLogFile}

// rootKeyHere reports whether <home>/fleet/root.key exists.
func (m *Manager) rootKeyHere() (bool, error) {
	_, err := os.Lstat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// FleetRootFiles returns the files of <home>/fleet (GET /fleet/root): what a
// master switch copies to the new master. On the acting master it first
// flushes its fleet branch, and refuses (unpublished_changes) while the
// branch holds commits main does not (flushBeforeSwitch): the new master
// takes over from main.
func (m *Manager) FleetRootFiles() (FleetRoot, error) {
	defer m.lockFleetOp()()
	dir := m.fleetRootDir()
	if ok, err := m.rootKeyHere(); err != nil {
		return FleetRoot{}, err
	} else if !ok {
		return FleetRoot{}, fleetErr(http.StatusNotFound, "no_root", "this instance holds no fleet root ("+filepath.Join(dir, pki.RootKeyFile)+" is absent)")
	}
	if ri := m.fleetRepo(); ri != nil {
		if err := m.flushBeforeSwitch(m.ctx, ri); err != nil {
			return FleetRoot{}, err
		}
	}
	out := FleetRoot{Dir: dir, Files: map[string]string{}}
	for _, name := range fleetRootFiles {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return FleetRoot{}, err
		}
		if !st.Mode().IsRegular() {
			return FleetRoot{}, fmt.Errorf("fleet root: %s is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return FleetRoot{}, err
		}
		out.Files[name] = string(b)
	}
	if c, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile)); err == nil {
		out.RootFingerprint, _ = pki.RootID(c)
	}
	return out, nil
}

// InstallFleetRoot installs the files of another master's <home>/fleet here
// (PUT /fleet/root), refused while a root key is here. The files land in a
// private staging directory, the key and certificate are checked to be one
// root (pki.LoadRoot), then the directory is moved into place.
func (m *Manager) InstallFleetRoot(files map[string]string) (FleetRoot, error) {
	defer m.lockFleetOp()()
	dir := m.fleetRootDir()
	present := func() error {
		return fleetErr(http.StatusConflict, "root_present", "a fleet root is already here ("+dir+"): stand down or decommission first")
	}
	// Anything in <home>/fleet, the root key first of all, refuses: the
	// files land by a rename, which never replaces a directory with content.
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return FleetRoot{}, present()
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return FleetRoot{}, err
	}
	known := map[string]bool{}
	for _, n := range fleetRootFiles {
		known[n] = true
	}
	for name := range files {
		if !known[name] {
			return FleetRoot{}, fleetErr(http.StatusUnprocessableEntity, "unknown_file", fmt.Sprintf("%q is not a fleet root file (want any of %s)", name, strings.Join(fleetRootFiles, ", ")))
		}
	}
	if files[pki.RootKeyFile] == "" || files[pki.RootCertFile] == "" {
		return FleetRoot{}, fleetErr(http.StatusUnprocessableEntity, "root_incomplete", pki.RootKeyFile+" and "+pki.RootCertFile+" are required")
	}
	staging := dir + ".receiving"
	if err := os.RemoveAll(staging); err != nil {
		return FleetRoot{}, err
	}
	if err := privdir.Ensure(staging); err != nil {
		return FleetRoot{}, err
	}
	if err := privdir.Verify(staging); err != nil {
		_ = os.RemoveAll(staging)
		return FleetRoot{}, fmt.Errorf("fleet root: refusing to write the root key: %w", err)
	}
	fail := func(err error) (FleetRoot, error) {
		_ = os.RemoveAll(staging)
		return FleetRoot{}, err
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o600); err != nil {
			return fail(err)
		}
	}
	root, err := pki.LoadRoot(staging)
	if err != nil {
		return fail(fleetErr(http.StatusUnprocessableEntity, "root_invalid", err.Error()))
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return fail(err)
	}
	id, _ := pki.RootID(root.Cert)
	log.Info().Str("dir", dir).Str("root", id).Msg("fleet: root installed; this instance is the master once the fleet's main names it (Host here, or a claim while main names no master)")
	return FleetRoot{Dir: dir, RootFingerprint: id, Files: map[string]string{}}, nil
}

// StandDown is DELETE /fleet/root, the first step of a master switch (user
// ruling 2026-10-16): this instance stops being the fleet's master. First it
// flushes (flushBeforeSwitch: everything its fleet branch holds reaches
// main, else 409 unpublished_changes). While it still does master work
// (actsAsFleetMaster) its last act as master is one commit on main
// (deleteFleetMaster: the master marker, fact.FleetMasterPath, deleted, so
// main shows a fleet with no acting master); then <home>/fleet,
// the root key with it, is deleted, so from here it merges, enrolls, revokes
// and writes no host record, and as the hub it takes no member push
// (FleetPushRefusal). The membership stays. A merge pass in flight finishes
// first, and none starts between the commit and the deletion.
//
// The new master then hosts the fleet (Host here), and checks only its own
// main for the marker. A master that does no master work (its copy follows
// another instance) writes nothing: it is not the host, and the host's judge
// refuses a member's change to the marker (the host-record rule).
//
// A failed publish of that commit refuses the stand-down
// (stand_down_unpublished) and keeps the root: a stand-down main never shows
// would leave the new master without the commit it checks for.
func (m *Manager) StandDown(ctx context.Context) error {
	defer m.lockFleetOp()()
	dir := m.fleetRootDir()
	if ok, err := m.rootKeyHere(); err != nil {
		return err
	} else if !ok {
		return fleetErr(http.StatusNotFound, "no_root", "this instance holds no fleet root ("+filepath.Join(dir, pki.RootKeyFile)+" is absent)")
	}
	if ri := m.fleetRepo(); ri != nil {
		if c := ri.consensus; c != nil {
			c.runMu.Lock()
			defer c.runMu.Unlock()
		}
		// Flush, then give up the role (user ruling): with the merger held
		// and fleetOpMu taken, nothing new enters the fleet branch.
		if err := m.flushBeforeSwitch(ctx, ri); err != nil {
			return err
		}
		acts := false
		if err := ri.WithRead(func(svc *store.Service) { acts = ri.actsAsFleetMaster(svc) }); err != nil {
			return err
		}
		if acts {
			if err := m.deleteFleetMaster(ctx, ri); err != nil {
				return fleetErr(http.StatusConflict, "stand_down_unpublished", "the commit that records the stand-down did not reach the fleet's main ("+credentialFreeErr(err).Error()+"): this instance is still the master. Try again")
			}
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	log.Info().Str("dir", dir).Msg("fleet: stood down as master; the root files are deleted here")
	return nil
}
