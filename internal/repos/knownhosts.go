package repos

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog/log"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// knownHostsMu serialises trust-on-first-use appends. The callback can run
// concurrently (parallel clones/syncs across repos), and two goroutines
// appending to the same file would interleave partial lines.
var knownHostsMu sync.Mutex

// hostKeyCallback returns an ssh.HostKeyCallback backed by an OpenSSH
// known_hosts file, with trust-on-first-use for hosts that are not yet listed.
//
// Trust model: an UNKNOWN host is accepted and recorded (TOFU) — the same
// choice OpenSSH offers interactively, which knomit cannot do because syncs run
// unattended. A host that is known but presents a DIFFERENT key is REJECTED:
// that is the MITM/key-substitution case the file exists to catch, and silently
// re-pinning would make the whole mechanism decorative.
//
// This replaces ssh.InsecureIgnoreHostKey(), which accepted every key forever.
func hostKeyCallback(path string) (gossh.HostKeyCallback, error) {
	if path == "" {
		return nil, fmt.Errorf("known_hosts path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("known_hosts dir: %w", err)
	}
	// knownhosts.New fails on a missing file; create it empty so the first
	// connection takes the TOFU path instead of erroring.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("known_hosts open: %w", err)
	}
	f.Close()

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		// Re-read on every call: a TOFU append by an earlier connection (or by
		// the user editing the file) must be visible without a restart.
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		verify, err := knownhosts.New(path)
		if err != nil {
			return fmt.Errorf("known_hosts parse (%s): %w", path, err)
		}

		err = verify(hostname, remote, key)
		if err == nil {
			return nil
		}

		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
			// len(Want) > 0 means the host IS known under a different key —
			// a key mismatch, not a first contact. Never auto-trust it.
			return err
		}

		// Unknown host: pin it.
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("known_hosts append: %w", err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			return fmt.Errorf("known_hosts append: %w", err)
		}
		log.Info().
			Str("host", hostname).
			Str("fingerprint", gossh.FingerprintSHA256(key)).
			Str("known_hosts", path).
			Msg("ssh: pinning host key on first use — verify this fingerprint out-of-band if the remote is untrusted")
		return nil
	}, nil
}
