package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"knomit/tools/bridge/knomitapi"
)

// runLogin is `kb login [--no-browser] <base-url>` and `kb logout <base-url>`.
// The token is saved under the knomit home for that host and sent by every
// kb request to it from then on; nothing is written to .mcp.json.
func runLogin(cmd string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("kb "+cmd, flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "print the authorization URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kb %s <base-url>", cmd)
	}
	base := fs.Arg(0)
	ctx := context.Background()
	if cmd == "logout" {
		if err := knomitapi.Logout(ctx, base); err != nil {
			return err
		}
		fmt.Fprintf(out, "logged out of %s\n", base)
		return nil
	}
	o := knomitapi.LoginOptions{Out: out, Confirm: askYesNo}
	if !*noBrowser {
		o.OpenBrowser = openBrowser
	}
	creds, err := knomitapi.Login(ctx, base, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "logged in to %s (scope %s)\n", creds.Issuer, creds.Scope)
	return nil
}

func askYesNo(q string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
}

func openBrowser(u string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "linux", "freebsd", "openbsd", "netbsd":
		c = exec.Command("xdg-open", u)
	default:
		return errors.New("no known way to open a browser here")
	}
	return c.Start()
}
