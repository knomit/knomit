//go:build desktop

package config

import (
	"sync"

	"github.com/rs/zerolog/log"
)

// applyBackupBuildPolicy switches replication OFF in the desktop build,
// unconditionally.
//
// # This is a project-owner ruling, not an implementation choice
//
// Backup is available for SERVER builds only. The desktop app must never
// trigger backup functionality at all. Do not re-litigate this by adding a
// desktop-visible toggle; if the ruling changes, change it here, deliberately.
//
// # Why it is a build tag and not a config default
//
// The hazard it removes is structural. The desktop app runs the knomit server
// in-process, so a user with `backup.enabled = true` in ~/.knomit/knomit.toml —
// set for their CLI server, which is the normal reason to have it — who then
// opens the desktop app would get a SECOND replicator against the same
// object-store prefix. Two litestream agents writing one LTX chain is the
// condition knomit deliberately never auto-repairs, because repairing it means
// discarding backup history.
//
// A default, or a check at one call site, would leave that reachable through an
// environment variable or a stray config file. Compiled out, it is not
// reachable at all: there is no value of KNOMIT_BACKUP_ENABLED and no TOML that
// turns replication on in a binary built with this tag. That is also why the
// desktop app does not take the KNOMIT_HOME claim (internal/homelock) — it
// cannot create the hazard the claim exists to catch, so guarding against it
// would be theatre.
//
// The override is LOUD when it actually changes something. Silently ignoring
// configuration is how an operator ends up believing their desktop data is
// backed up; once per process is enough to make it discoverable in the log the
// desktop app already writes, and Load runs more than once in some paths.
func applyBackupBuildPolicy(cfg *Config) {
	if !cfg.Backup.Enabled {
		return
	}
	cfg.Backup.Enabled = false
	desktopBackupOnce.Do(func() {
		log.Warn().Str("configured_url", cfg.Backup.URL).
			Msg("backup is configured but IGNORED: this is the desktop build, and replication is " +
				"available for server builds only. Nothing here is being backed up to the replica.")
	})
}

// desktopBackupOnce keeps the override from repeating on every config.Load.
var desktopBackupOnce sync.Once
