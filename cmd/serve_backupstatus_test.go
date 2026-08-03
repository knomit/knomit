package cmd

import (
	"reflect"
	"testing"

	"knomit/internal/backup"
	"knomit/internal/obs/diag"
)

// TestBackupStatusHookIsNilWhenBackupIsDisabled: a nil *Manager is how "backup
// is off" is spelled everywhere in knomit, and the diagnostics port reads a nil
// HOOK as the same thing. Handing it a working hook over a nil manager would
// publish an empty backup block on an instance that has no backup — an
// all-clear for a feature that is not running.
func TestBackupStatusHookIsNilWhenBackupIsDisabled(t *testing.T) {
	if backupStatusHook(nil) != nil {
		t.Fatal("backupStatusHook(nil) returned a hook; /runtime/status would carry an empty backup block")
	}
}

// TestBackupStatusMirrorCarriesEveryField guards the copy in backupStatusHook
// against the failure it invites: a field added to backup.DBStatus and to the
// diag mirror, but not to the assignment between them, which silently
// reports a zero for it forever.
func TestBackupStatusMirrorCarriesEveryField(t *testing.T) {
	src := reflect.TypeOf(backup.DBStatus{})
	dst := reflect.TypeOf(diag.BackupDBStatus{})
	if src.NumField() != dst.NumField() {
		t.Fatalf("backup.DBStatus has %d fields and diag.BackupDBStatus has %d; the mirror has drifted",
			src.NumField(), dst.NumField())
	}
	for i := range src.NumField() {
		s, d := src.Field(i), dst.Field(i)
		if s.Name != d.Name || s.Type != d.Type {
			t.Errorf("field %d: backup has %s %s, diag has %s %s", i, s.Name, s.Type, d.Name, d.Type)
		}
	}
}
