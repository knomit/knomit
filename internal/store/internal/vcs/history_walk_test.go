package vcs

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentIDOfAuthor(t *testing.T) {
	require.Equal(t, "research-box", AgentIDOfAuthor("research-box+learn@agents.knomit.io"))
	require.Equal(t, "mindev-local-8ef0cd32", AgentIDOfAuthor("mindev-local-8ef0cd32@agents.knomit.io"))
	require.Equal(t, "", AgentIDOfAuthor("someone@example.com"))
	require.False(t, strings.Contains(AgentIDOfAuthor("a+b+c@agents.knomit.io"), "+"))
}
