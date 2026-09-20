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
// success is how you break redirection. Measured on this binary, bytes landing
// in the destination over 3 runs each:
//
//	launch                          handle       AttachConsole   bytes
//	bare, no redirection            0x0 UNKNOWN  succeeds        n/a (console)
//	cmd.exe   `... > out.txt`       valid DISK   succeeds        15, 15, 15
//	PowerShell `... | Set-Content`  valid PIPE   succeeds        16, 16, 16
//	PowerShell `... > out.txt`      valid PIPE   succeeds        0, 0, 0
//
// Two separate things are going on, and conflating them is how this gets
// written wrong:
//
// THE ATTACH IS NOT THE SIGNAL. AttachConsole succeeds in every row, because
// the parent shell has a console in every row. Only the bare row has an
// unusable stdout. So the per-stream guard keys on the HANDLE: an
// unconditional switch to CONOUT$ would send cmd's `> out.txt` to a console
// nobody is looking at and leave the file empty — a silent wrong answer rather
// than a visible failure. A stream that already works is left exactly as it is.
//
// THE LAST ROW IS NOT OURS TO FIX. PowerShell (both 5.1 and 7.x) does not wait
// on a GUI-subsystem child: it gives it a PIPE rather than the file handle and
// closes the read end without draining, so the bytes are discarded whether or
// not the write reports success. CONOUT$ would not put them in the file either,
// and the same shells redirect a CONSOLE-subsystem binary (knomit-okf.exe)
// to a file correctly — so this is PowerShell's process handling of GUI
// children, not a defect in the guard below. Documented in README.md as
// "use a pipe or cmd /c, not `>`"; there is deliberately no code here for it.
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
