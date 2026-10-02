//go:build windows

package serveraddr

import (
	"errors"
	"strings"

	"knomit/internal/auth"
)

const (
	// localScheme names this platform's local listener: a named pipe, in the
	// spelling Docker uses for one (npipe:////./pipe/<name>).
	localScheme = "npipe"
	// otherLocalScheme is the other platform's, refused here by name: the
	// server's local listener on Windows is a pipe, never AF_UNIX.
	otherLocalScheme = "unix"
	localForm        = `npipe:////./pipe/<name>`
)

// parseLocal reads what follows "npipe://": "//./pipe/<name>", with forward
// slashes standing for the pipe name's backslashes.
func parseLocal(rest string) (string, error) {
	if rest == "" {
		return "", errors.New("no pipe name")
	}
	p := strings.ReplaceAll(rest, "/", `\`)
	if !strings.HasPrefix(p, auth.PipePrefix) || len(p) == len(auth.PipePrefix) {
		return "", errors.New("want a pipe name: " + localForm)
	}
	return p, nil
}

func formatLocal(path string) string {
	return localScheme + "://" + strings.ReplaceAll(path, `\`, "/")
}
