package events

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/remotesync"
)

// Pure: the wire vocabulary of one status column. NULL is "never ran", not
// "ok" — a dot must not claim health for a half that has not been tried.
func TestRemoteStatusValue(t *testing.T) {
	ok, bad := "ok", "error"
	require.Equal(t, "", RemoteStatusValue(nil))
	require.Equal(t, "ok", RemoteStatusValue(&ok))
	require.Equal(t, "error", RemoteStatusValue(&bad))
	s, p := RemoteHealth(&remotesync.Remote{LastStatus: &ok})
	require.Equal(t, [2]string{"ok", ""}, [2]string{s, p})
}
