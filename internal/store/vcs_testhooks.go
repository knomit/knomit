package store

import (
	"golang.org/x/crypto/ssh"

	"knomit/internal/store/internal/vcs"
)

// The vcs child's test-binary-only installers, forwarded under their public
// store names for the test packages that call them. Both refuse outside a
// test binary (testing.Testing) in the child; test/archtest keeps production
// source from naming them, with this file and the child's definitions as the
// only allowed non-test files.

// SetTestFallbackSigner is vcs.SetTestFallbackSigner.
func SetTestFallbackSigner(signer ssh.Signer) { vcs.SetTestFallbackSigner(signer) }

// SetTestFallbackAgentID is vcs.SetTestFallbackAgentID.
func SetTestFallbackAgentID(f func(branch string) string) { vcs.SetTestFallbackAgentID(f) }
