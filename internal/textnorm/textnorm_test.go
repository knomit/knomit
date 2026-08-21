package textnorm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalize(t *testing.T) {
	require.Equal(t, "ai governance", Canonicalize("AI-Governance"))
	require.Equal(t, "ai governance", Canonicalize("  AI  Governance  "))
	require.Equal(t, "multi tenant/auth", Canonicalize("Multi-Tenant/Auth"))
	// Underscores survive: identifier-like tags stay one token.
	require.Equal(t, "commit_log", Canonicalize("commit_log"))
	// Idempotent — it is applied at both index and query time.
	require.Equal(t, Canonicalize("AI-Governance"), Canonicalize(Canonicalize("AI-Governance")))
}

func TestFold(t *testing.T) {
	require.Equal(t, Fold("Antigravity"), Fold("ANTIGRAVITY"))
	// Fold does NOT de-hyphenize — that is the whole reason it is separate
	// from Canonicalize. It mirrors a COLLATE NOCASE column.
	require.Equal(t, "multi-tenant", Fold("Multi-Tenant"))
}

func TestStem_IrregularsAndGuards(t *testing.T) {
	for _, pair := range [][2]string{
		{"vulnerability", "vulnerabilities"},
		{"analysis", "analyses"},
		{"index", "indices"},
		{"matrix", "matrices"},
		{"thesis", "theses"},
	} {
		require.Equalf(t, Stem(pair[0]), Stem(pair[1]),
			"%q and %q must collapse to one key", pair[0], pair[1])
	}

	// Guards: short tokens and -ics words merely END in 's' and are never
	// plurals. Over-singularizing them would mangle the key asymmetrically.
	for _, tok := range []string{"ai", "aws", "llm", "tls", "ops"} {
		require.Equal(t, tok, Stem(tok))
	}
	for _, tok := range []string{"metrics", "economics", "robotics", "ethics"} {
		require.Equal(t, tok, Stem(tok))
	}

	// Idempotent.
	require.Equal(t, Stem("vulnerabilities"), Stem(Stem("vulnerabilities")))
}

func TestTokens_SplitsOnSlashAndSpace(t *testing.T) {
	require.Equal(t, []string{"multi", "tenant", "auth"},
		Tokens(Canonicalize("multi-tenant/auth")))
}

func TestTokens_DeduplicatesPreservingFirstAppearance(t *testing.T) {
	require.Equal(t, []string{"auth", "token"},
		Tokens(Canonicalize("auth/token/auth")))
}

func TestTokens_Empty(t *testing.T) {
	require.Empty(t, Tokens(Canonicalize("")))
	require.Empty(t, Tokens(Canonicalize("   ")))
}
