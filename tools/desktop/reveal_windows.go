//go:build desktop && windows

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// revealInFileManager opens Explorer on the file's folder with the file
// selected — the same thing macOS gets from `open -R`, and better than the
// freedesktop fallback of opening the directory and leaving the user to find
// the file.
//
// Four things here are load-bearing, and three of them were established by
// running it rather than by reading docs:
//
// SysProcAttr.CmdLine, not arguments. Go builds a command line from args with
// syscall.EscapeArg, which quotes any argument containing a space — producing
// `"/select,C:\some dir\my log.txt"`. Explorer's parser does not accept the
// whole token quoted and silently opens the user's Documents folder instead,
// which looks like the feature ignoring its input. CmdLine bypasses that and
// sends the exact string Explorer wants, with quotes around the PATH only.
//
// CmdLine replaces the ENTIRE command line, argv[0] included, so it has to
// start with the executable name. exec.Command is given no arguments on
// purpose: CmdLine overrides them, so any argument listed there would be dead
// code that misleads the next reader.
//
// Start, never Run. explorer.exe exits with status 1 on SUCCESS — it hands the
// request to the already-running shell process and terminates. Run reports
// that as a failure and the UI would show an error over a window that had just
// opened correctly.
//
// The file must EXIST when Explorer runs. /select on a missing path falls back
// to opening the parent with nothing selected, which matters here because the
// log file is created lazily — reveal before the first log line writes lands
// on an empty-looking folder.
func revealInFileManager(path string) error {
	clean := filepath.FromSlash(filepath.Clean(path))

	// Explorer's command-line parser has no escape for a double quote, so a
	// path containing one cannot be expressed. Refuse rather than build a
	// command line that means something other than what was asked. A double
	// quote is not a legal character in a Windows filename, so this is
	// unreachable for a real path and exists to keep the string below honest.
	if strings.Contains(clean, `"`) {
		return fmt.Errorf("cannot reveal a path containing a double quote: %q", clean)
	}

	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + clean + `"`}
	return cmd.Start()
}
