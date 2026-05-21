//go:build sqlite_vtable

package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVTabRepoRegistry_BindLookupUnbind(t *testing.T) {
	// Sentinel repoHandler value: zero-valued struct is fine for identity testing.
	rh := &repoHandler{}
	const path = "/tmp/vtab_registry_test.db"

	require.Nil(t, lookupVTabRepo(path), "lookup before bind must return nil")

	bindVTabRepo(path, rh)
	require.Same(t, rh, lookupVTabRepo(path), "lookup after bind must return the same handle")

	unbindVTabRepo(path)
	require.Nil(t, lookupVTabRepo(path), "lookup after unbind must return nil")
}
