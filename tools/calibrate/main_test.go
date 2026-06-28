package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewRootCmd verifies that newRootCmd returns a non-nil cobra command.
func TestNewRootCmd(t *testing.T) {
	cmd := newRootCmd()
	require.NotNil(t, cmd)
}

// TestEmbeddingsHelp verifies that "calibrate embeddings --help" lists the
// three expected flags: --cache, --models, --baseline.
func TestEmbeddingsHelp(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"embeddings", "--help"})

	err := cmd.Execute()
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "--cache", "help should list --cache flag")
	assert.Contains(t, out, "--models", "help should list --models flag")
	assert.Contains(t, out, "--baseline", "help should list --baseline flag")
}

// TestEmbeddingsMissingCache verifies that running "embeddings" without
// --cache (a required flag) returns an error.
func TestEmbeddingsMissingCache(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"embeddings", "somefile.db"})

	err := cmd.Execute()
	require.Error(t, err, "missing required --cache should produce an error")
	assert.True(t,
		strings.Contains(err.Error(), "cache") || strings.Contains(buf.String(), "cache"),
		"error should mention 'cache'",
	)
}

// TestEmbeddingsMissingDBPaths verifies that running "embeddings" with --cache
// set but ZERO positional db paths returns an error (cobra.MinimumNArgs(1)),
// rejected before RunE so it never reaches ONNX.
func TestEmbeddingsMissingDBPaths(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"embeddings", "--cache", "/tmp/c"})

	err := cmd.Execute()
	require.Error(t, err, "zero positional db paths should produce an error")
}

// TestUnknownSubcommand verifies that an unknown subcommand returns an error.
func TestUnknownSubcommand(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"no-such-subcommand"})

	err := cmd.Execute()
	require.Error(t, err, "unknown subcommand should produce an error")
}
