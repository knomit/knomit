//go:build !windows

package serveraddr

import (
	"errors"
	"fmt"
	"path/filepath"

	"knomit/internal/auth"
)

const (
	// localScheme names this platform's local listener: a unix socket.
	localScheme = "unix"
	// otherLocalScheme is the other platform's, refused here by name.
	otherLocalScheme = "npipe"
	localForm        = "unix:///absolute/path/knomit.sock"
)

// parseLocal reads what follows "unix://". It is taken LITERALLY — no
// percent-decoding — so a data root with a space or a '%' round-trips
// exactly. It must be absolute: "unix://relative" (host "relative") would
// otherwise resolve against each process's working directory.
func parseLocal(rest string) (string, error) {
	if rest == "" {
		return "", errors.New("no socket path")
	}
	if !filepath.IsAbs(rest) {
		return "", errors.New("the socket path must be absolute: unix:///absolute/path")
	}
	// The same cap auth.ListenLocal enforces: a longer path cannot be a unix
	// socket address at all, and dialling it fails with a bare "invalid
	// argument" that names neither the cause nor the limit.
	if n, limit := len(rest), auth.SunPathCap(); n >= limit {
		return "", fmt.Errorf("the socket path is %d bytes; a unix socket path on this platform must be under %d "+
			"(name the socket the server actually listens on: it logs it at startup)", n, limit)
	}
	return rest, nil
}

func formatLocal(path string) string { return localScheme + "://" + path }
