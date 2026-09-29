package knomitapi

import "knomit/internal/serverkey"

// The MCP-config server key lives in internal/serverkey since F07 PR 5: the
// trigger dispatcher (internal/repos) builds the same MCP configuration for a
// recipe as `kb claude init` writes, and internal/ must not import tools/.
// These names keep the bridge's API unchanged; see internal/serverkey for the
// rules.

// ServerKey is serverkey.ServerKey.
func ServerKey(repoName, lens string) string { return serverkey.ServerKey(repoName, lens) }

// UnboundServerKey is serverkey.UnboundServerKey.
func UnboundServerKey(dirName string) string { return serverkey.UnboundServerKey(dirName) }

// MaxServerKeyLen is serverkey.MaxServerKeyLen.
const MaxServerKeyLen = serverkey.MaxServerKeyLen

// MaxScopeNameLen is serverkey.MaxScopeNameLen.
const MaxScopeNameLen = serverkey.MaxScopeNameLen
