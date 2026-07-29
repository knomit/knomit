package config

import (
	"testing"
	"time"
)

// TestDefaults_BackupStatusCacheTTL: the operational surface must never poll
// the replica uncapped, so this has a real default rather than a zero that
// would read as "no cache".
func TestDefaults_BackupStatusCacheTTL(t *testing.T) {
	if got := Defaults().Backup.StatusCacheTTL; got <= 0 {
		t.Fatalf("Backup.StatusCacheTTL default = %v, want a positive TTL", got)
	}
}

func TestLoad_BackupStatusCacheTTLFromEnv(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_BACKUP_STATUS_CACHE_TTL", "90s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backup.StatusCacheTTL != 90*time.Second {
		t.Errorf("Backup.StatusCacheTTL = %v, want 90s", cfg.Backup.StatusCacheTTL)
	}
}
