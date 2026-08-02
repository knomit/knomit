package cmd

import (
	"context"

	"knomit/internal/backup"
	"knomit/internal/runtimeobs"
)

// This file wires `knomit serve` to the diagnostics port. It defines NO
// command — the serve_ prefix says so, matching serve_track.go. It was called
// backupstatus.go, which sat beside reset.go and verify.go and read as a
// `knomit backup-status` command that has never existed.
//
// backupStatusHook adapts backup.Manager.Status to the diagnostics port's local
// mirror type, and returns nil when replication is disabled.
//
// The adapter exists so internal/runtimeobs never imports internal/backup: the
// diagnostics port is meant to be usable by anything, and a dependency on the
// replication client would drag the agent protocol into every consumer of it.
// The copy below is the seam, and cmd is the right place for it because cmd is
// where the two are already both in scope.
//
// Returning a nil FUNCTION rather than a function returning nil is the whole
// contract with the port: nil means "backup is off", which omits the block from
// /runtime/status and the series from /metrics. A non-nil hook over a nil
// Manager would report a permanently empty backup surface on an instance that
// has no backup — the shape of an all-clear.
func backupStatusHook(m *backup.Manager) func(context.Context) []runtimeobs.BackupDBStatus {
	if m == nil {
		return nil
	}
	return func(ctx context.Context) []runtimeobs.BackupDBStatus {
		src := m.Status(ctx)
		out := make([]runtimeobs.BackupDBStatus, 0, len(src))
		for _, st := range src {
			out = append(out, runtimeobs.BackupDBStatus{
				Name:         st.Name,
				LocalTXID:    st.LocalTXID,
				RemoteTXID:   st.RemoteTXID,
				InSync:       st.InSync,
				LastSyncUnix: st.LastSyncUnix,
				Paused:       st.Paused,
				LastError:    st.LastError,
			})
		}
		return out
	}
}
