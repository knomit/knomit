//go:build !windows

package serveraddr

import (
	"errors"
	"path/filepath"
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
	return rest, nil
}

func formatLocal(path string) string { return localScheme + "://" + path }
