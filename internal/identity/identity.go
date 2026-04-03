package identity

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

// EnsureKeyPair loads or generates an Ed25519 keypair at the given path.
// If the key does not exist, it is generated and the public key is logged to stderr.
// Returns the signer and the key's short fingerprint (first 8 hex chars of SHA256 of public key).
func EnsureKeyPair(path string) (ssh.Signer, string, error) {
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

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}
	comment := "knomit@" + hostname
	pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	pubLine = strings.TrimSpace(pubLine) + " " + comment + "\n"

	pubPath := path + ".pub"
	if err := os.WriteFile(pubPath, []byte(pubLine), 0644); err != nil {
		return nil, "", fmt.Errorf("write public key: %w", err)
	}

	fp := fingerprint(signer.PublicKey())
	log.Info().Str("fingerprint", fp).Str("pub", pubPath).Msg("generated new SSH keypair")
	fmt.Fprintf(os.Stderr, "New knomit SSH public key: %s\n", strings.TrimSpace(pubLine))

	return signer, fp, nil
}

// fingerprint returns the first 8 hex chars of SHA256 of the public key.
func fingerprint(pub ssh.PublicKey) string {
	h := sha256.Sum256(pub.Marshal())
	return hex.EncodeToString(h[:])[:8]
}

// AgentBranch returns the agent branch name for this machine and key fingerprint.
// Format: agent/<sanitized-hostname>-<fingerprint8>
func AgentBranch(fp string) string {
	hostname, _ := os.Hostname()
	return "agent/" + SanitizeHostname(hostname) + "-" + fp
}

// SanitizeHostname replaces chars invalid in git ref names with "-".
// Falls back to "local" if hostname is empty.
func SanitizeHostname(hostname string) string {
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
