//go:build windows

package crashdump

import (
	"os"

	"golang.org/x/sys/windows"
)

// dupToStderr points the process's error output at f.
//
// Windows has no dup2, and the two things that write to "stderr" reach it by
// different routes, so both have to be moved:
//
//   - The Go RUNTIME writes its fatal traceback — and the CGO/ONNX SIGSEGV
//     traceback this whole package exists to capture — by calling
//     GetStdHandle(STD_ERROR_HANDLE) on each write. SetStdHandle changes what
//     that returns, so the traceback follows.
//
//   - os.Stderr does NOT. It is built once during package init from the
//     handle GetStdHandle returned then, and it keeps writing to that handle
//     no matter what SetStdHandle is told afterwards. Hence the assignment.
//
// Missing the second half is what TestRedirectStderrSubprocess catches: the
// runtime traceback would be captured while an ordinary os.Stderr write from
// the same process went to the console.
//
// Assigning os.Stderr is a process-global mutation and is safe only because
// RedirectStderr runs during startup, before anything else is logging. It is
// no more global than the Unix dup2, which redirects every writer of fd 2
// including os.Stderr; this just has to say so out loud.
//
// The previous STD_ERROR_HANDLE is deliberately not closed. It may be the
// console the user is watching or a handle the parent process owns, and this
// function has no way to tell; the Unix side gives up the old fd 2 only
// because dup2 does it implicitly.
func dupToStderr(f *os.File) error {
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())); err != nil {
		return err
	}
	os.Stderr = f
	return nil
}
