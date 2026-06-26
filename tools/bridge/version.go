package main

import (
	"fmt"
	"io"

	"knomit/internal/version"
)

// runVersion handles the `knomit-bridge version` subcommand. It reports
// whether it consumed the args; when it did, it has already written the build
// version to out. Mirrors the early-dispatch style of the `claude` subcommand.
func runVersion(args []string, out io.Writer) (handled bool) {
	if len(args) >= 1 && args[0] == "version" {
		fmt.Fprintln(out, version.String())
		return true
	}
	return false
}
