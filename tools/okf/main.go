// knomit-okf turns a knomit knowledge base into a publishable OKF repository,
// and keeps it in sync, using nothing but a git URL.
//
//	knomit-okf clone https://github.com/knomit/knomit-kb foobar
//	cd foobar
//	git remote add origin git@github.com:me/my-kb-okf.git && git push -u origin main
//
//	# later — by you, or by anyone who cloned your published repo
//	knomit-okf sync
//	git push
//
// The output directory IS the export: there is no second checkout, no cache,
// and no state held by a knomit server. The knowledge base's own history is
// fetched into the same .git under refs/knomit-okf/source/*, which is outside
// refs/heads/* and therefore never checked out and never pushed by git's
// default refspec.
//
// The one leak path is an explicit `git push --mirror` or a `refs/*` refspec,
// which would publish the source refs. That is not defended against in code.
//
// knomit-okf never pushes. It commits; you push — your remote, your
// credentials, your cadence.
package main

import (
	"fmt"
	"io"
	"os"

	"knomit/internal/version"
)

const usage = `knomit-okf — publish a knomit knowledge base as an OKF repository

usage:
  knomit-okf clone [-b <branch>] [--publish-source] <kb-url> <dir>
  knomit-okf sync  [-b <branch>] [--source <url>] [--publish-source]
  knomit-okf version

clone creates <dir> as an OKF repository for <kb-url>. sync runs inside one and
updates it from the knowledge base. Neither pushes — you push.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "knomit-okf: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return nil
	}
	switch args[0] {
	case "clone":
		return runClone(args[1:], out)
	case "sync":
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		return runSync(args[1:], dir, out)
	case "version", "--version", "-version":
		fmt.Fprintln(out, "knomit-okf "+version.String())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}
