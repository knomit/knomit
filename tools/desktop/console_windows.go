//go:build desktop && windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// attachParentProcess is ATTACH_PARENT_PROCESS: the sentinel process id that
// asks AttachConsole for the console of whoever launched us, rather than a
// specific pid. It is (DWORD)-1, so it has to be written as an all-ones
// uintptr — a literal -1 is not a legal argument to LazyProc.Call.
const attachParentProcess = ^uintptr(0)

// procAttachConsole is kernel32!AttachConsole, reached through a lazy proc
// because golang.org/x/sys/windows v0.46.0 has no wrapper for it (it has
// SetStdHandle and the STD_* handle constants, but nothing that attaches a
// console). Resolved lazily, so the DLL is only touched on the path that needs it.
var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
)

// attachParentConsole gives os.Stdout and os.Stderr somewhere to go when this
// process was started with nowhere to write, by borrowing the console of
// whoever launched it. It is called on the `version` path only.
//
// The desktop binary is linked -H windowsgui (see DESKTOP_LDFLAGS in the
// Makefile), so Windows gives it no console of its own. That is the point for a
// tray app — a GUI that flashes up a console window is a bug, and a console
// would hand the launching terminal ownership of our lifetime — but it also
// means `knomit-desktop.exe --version` typed at a prompt prints somewhere the
// user cannot see. This repairs that one line.
//
// What makes this fiddlier than "attach a console and print" is that
// AttachConsole SUCCEEDS in cases where nothing is wrong, and acting on that
// success is how you break redirection. Measured, on this binary, across three
// launches:
//
//	launch              STD_OUTPUT_HANDLE  GetFileType   AttachConsole
//	no redirection      0x0                0 (UNKNOWN)   succeeds
//	`... > out.txt`     valid              1 (DISK)      succeeds
//	`... | something`   valid              3 (PIPE)      succeeds
//
// Only the first row is broken: a GUI-subsystem process DOES inherit a working
// stdout when the shell redirects one, and it is the same AttachConsole success
// in every row. So the signal to act on is the HANDLE, not the attach — an
// unconditional switch to CONOUT$ sends `--version > out.txt` to a console
// nobody is looking at and leaves the file empty, which is a silent wrong answer
// rather than a visible failure. Hence the per-stream guard: a stream that
// already works is left exactly as it is.
//
// AttachConsole alone would not be enough for the broken row either. It gives
// the process a console but does not repair the handles Go captured at startup,
// so CONOUT$ — the console's own output device — has to be opened and installed
// over os.Stdout/os.Stderr for fmt and log to reach it. SetStdHandle
// additionally fixes the raw handle, for anything that asks Windows instead of Go.
//
// Failure is ignored throughout, because every way this fails is ordinary rather
// than exceptional: launched from Explorer, the tray or a login item there is no
// parent console to attach to. There is also nowhere to report a failure TO —
// the only surface would be the console we just failed to get.
func attachParentConsole() {
	outBroken, errBroken := !writable(os.Stdout), !writable(os.Stderr)
	if !outBroken && !errBroken {
		return
	}
	if r1, _, _ := procAttachConsole.Call(attachParentProcess); r1 == 0 {
		return
	}
	con, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return
	}
	if outBroken {
		os.Stdout = con
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(con.Fd()))
	}
	if errBroken {
		os.Stderr = con
		_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(con.Fd()))
	}
}

// writable reports whether f's handle is one this process can actually write to.
// Stat is the cheap way to ask: it calls GetFileType underneath, which is the
// exact call that distinguishes the three rows in the table above — it fails
// with "the handle is invalid" on the handle a console-less GUI process is
// given, and succeeds for a file or a pipe.
func writable(f *os.File) bool {
	_, err := f.Stat()
	return err == nil
}
