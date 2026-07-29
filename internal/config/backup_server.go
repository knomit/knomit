//go:build !desktop

package config

// applyBackupBuildPolicy is a no-op in server builds: replication is theirs to
// have, and backup.enabled decides it as configured.
//
// The desktop counterpart in backup_desktop.go forces it off, and carries the
// reasoning and the project-owner ruling behind it.
func applyBackupBuildPolicy(*Config) {}
