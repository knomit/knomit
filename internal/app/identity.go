package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
)

// ensureKeyPair loads or generates an Ed25519 keypair at the given path.
// If the key does not exist, it is generated and the public key is logged to stderr.
// Returns the signer and the key's short fingerprint (first 8 hex chars of SHA256 of public key).
func ensureKeyPair(path string) (ssh.Signer, string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, "", fmt.Errorf("parse private key %s: %w", path, err)
		}
		fp := fingerprint(signer.PublicKey())
		return signer, fp, nil
	}

	if !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("read key %s: %w", path, err)
	}

	// Generate new key
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, "", fmt.Errorf("mkdir for key: %w", err)
	}

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	pemBlock, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, "", fmt.Errorf("marshal private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)

	// Write private key atomically
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, pemBytes, 0600); err != nil {
		return nil, "", fmt.Errorf("write temp key: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("rename key: %w", err)
	}

	// Write public key
	signer, err := ssh.NewSignerFromKey(privKey)
	if err != nil {
		return nil, "", fmt.Errorf("create signer: %w", err)
	}

	pubLine := authorizedKeyLine(signer.PublicKey()) + "\n"

	pubPath := path + ".pub"
	if err := os.WriteFile(pubPath, []byte(pubLine), 0644); err != nil {
		return nil, "", fmt.Errorf("write public key: %w", err)
	}

	fp := fingerprint(signer.PublicKey())
	log.Info().Str("fingerprint", fp).Str("pub", pubPath).Msg("generated new SSH keypair")
	fmt.Fprintf(os.Stderr, "New knomit SSH public key: %s\n", strings.TrimSpace(pubLine))

	return signer, fp, nil
}

// authorizedKeyLine is the instance's public key as one authorized_keys line
// with the "knomit@<hostname>" comment, which `knomit identity enroll`
// reads the certificate's host label from. ONE builder, so the .pub that
// ensureKeyPair writes and PublicKeyLine cannot differ.
func authorizedKeyLine(pub ssh.PublicKey) string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " knomit@" + hostname
}

// PublicKeyLine returns the line to hand the fleet operator for `knomit
// identity enroll --pubkey`: the one ensureKeyPair wrote to <keyPath>.pub,
// without its newline. It is derived from the PRIVATE key at keyPath, not
// read from .pub, which can be stale or — for a [remote].ssh_key — absent.
func PublicKeyLine(keyPath string) (string, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return "", fmt.Errorf("parse private key %s: %w", keyPath, err)
	}
	return authorizedKeyLine(signer.PublicKey()), nil
}

// fingerprint returns the first 8 hex chars of SHA256 of the public key.
func fingerprint(pub ssh.PublicKey) string {
	h := sha256.Sum256(pub.Marshal())
	return hex.EncodeToString(h[:])[:8]
}

// agentBranch returns the agent branch name for this machine and key fingerprint.
// Format: agent/<sanitized-hostname>-<fingerprint8>
func agentBranch(fp string) string {
	hostname, _ := os.Hostname()
	return "agent/" + sanitizeHostname(hostname) + "-" + fp
}

// sanitizeHostname replaces chars invalid in git ref names with "-".
// Falls back to "local" if hostname is empty.
func sanitizeHostname(hostname string) string {
	if hostname == "" {
		return "local"
	}
	replacer := strings.NewReplacer(
		" ", "-",
		"~", "-",
		"^", "-",
		":", "-",
		"?", "-",
		"*", "-",
		"[", "-",
		"\\", "-",
	)
	return replacer.Replace(hostname)
}

// AgentSlug maps an agent branch name to the slug form used as a category key:
// "agent/mindev.local-8ef0cd32" becomes "mindev-local-8ef0cd32".
//
// Exported because skills and the fleet executor must compute the SAME slug
// this process does. A signal is addressed by its path, so two derivations that
// disagreed would not error — they would quietly address two different
// categories, and one side's messages would never be read.
//
// This is the ONLY place a branch name is normalised to [a-z0-9-], and it is
// deliberately not folded into sanitizeHostname. That function replaces just
// the characters git rejects in a ref name, so dots, underscores and uppercase
// letters all survive into the branch: "mindev.local" is still "mindev.local"
// there, and a caller who assumes otherwise gets a slug with a dot in it.
//
// The "agent/" prefix is stripped first so the separator does not survive as a
// hyphen. A name without that prefix is slugified as-is rather than rejected —
// callers hold branch names from several sources, and a slug is a key, not a
// validation.
//
// Returns "" for an empty branch and for one with no [a-z0-9] rune at all
// (e.g. "agent/..."). A caller building a path such as inbox/<slug>/ must treat
// "" as invalid and refuse rather than address the parent directory.
func AgentSlug(branch string) string {
	// Lowercased BEFORE the prefix is stripped: TrimPrefix is case-sensitive,
	// so "Agent/host-abc" would otherwise keep its prefix and slug to
	// "agent-host-abc" — a different key for the same branch.
	s := strings.TrimPrefix(strings.ToLower(branch), "agent/")

	var b strings.Builder
	b.Grow(len(s))
	prevHyphen := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		// Every other rune — dot, underscore, slash, multi-byte — collapses
		// into a single hyphen, so a hostname already sanitized by
		// sanitizeHostname does not come out with runs of them.
		if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}
