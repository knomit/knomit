// knomit-relnotes turns a commit range into release notes.
//
//	relnotes changes -from <rev> -to <rev>   # deterministic, grouped Markdown
//	relnotes distill                          # stdin -> "## What's new", best effort
//	relnotes version
//
// The two halves have deliberately opposite failure modes. `changes` is the
// changelog and fails loudly if it cannot produce one. `distill` is a nicety
// layered on top: it exits 0 having written nothing when anything at all goes
// wrong, so a missing API key, a quota trip, or a transport error degrades the
// notes instead of failing a release.
package main

import (
	"fmt"
	"os"

	"knomit/internal/version"
)

const usage = `knomit relnotes — build release notes from a commit range

usage:
  relnotes changes -from <rev> -to <rev>
  relnotes distill
  relnotes version

changes walks the merge commits between the two revisions, resolves each pull
request through gh, and renders Markdown grouped by conventional-commit type.
Non-merge commits reachable from no pull request are listed separately so work
pushed straight to the branch is never silently dropped.

distill reads that Markdown on stdin and writes a short "## What's new" section
using $GEMINI_API_KEY. With no key, or on any error, it writes nothing and
exits 0 — the caller keeps whatever it already had.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "changes":
		err = runChanges(os.Args[2:])
	case "distill":
		err = runDistill(os.Args[2:])
	case "version":
		fmt.Println(version.String())
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes:", err)
		os.Exit(1)
	}
}
