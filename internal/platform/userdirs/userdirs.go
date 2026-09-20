// Package userdirs answers where an OS keeps a user's per-user directories.
//
// It is pure OS knowledge and deliberately knows no application: it returns
// BASE directories, never a path with a product's folder name joined onto it.
// That split is the reason it can live in internal/platform at all — the tier
// is defined by knowing "the OS and the binary, and nothing about this
// application" (see TestPlatformKnowsNothingAboutKnomit), and a base directory
// is exactly that.
//
// The application knowledge — what the folder inside these directories is
// called — belongs to the caller, which joins it on. One caller, one spelling;
// nothing here needs to be told the name, so nothing here has to take it as a
// parameter.
//
// WHAT IS LOAD-BEARING HERE, and it is not the paths. Every function returns
// ("", error) rather than a best guess when it cannot resolve a directory. The
// bug this package was extracted around was `home, _ := os.UserHomeDir()`
// followed by string concatenation: on Windows a discarded error does not
// degrade to "no home", it degrades to a relative path that the OS resolves
// against the CURRENT DRIVE — producing C:\<name>, a writable, plausible and
// entirely wrong directory that a real aborted launch populated with a keypair
// and a partial download. A caller that logs an error and carries on must not
// be handed a path alongside it.
//
// Errors describe the OS situation only ("%LOCALAPPDATA% is unset", "$HOME is
// unset") and name the variables an operator can set. They do NOT name an
// application's override, because this package does not know there is one;
// callers with such an override wrap these errors to add it.
package userdirs

// StateDir returns the per-user directory this OS designates for application
// state: files that describe THIS machine and are not worth roaming or backing
// up. The directory is NOT created, and no application name is joined on.
//
// Per OS: %LOCALAPPDATA% on Windows, ~/Library/Application Support on macOS,
// $XDG_STATE_HOME (or ~/.local/state) on Linux. Elsewhere it is an error —
// there is no designated answer to give, and inventing one is the failure mode
// this package exists to prevent.
func StateDir() (string, error) { return stateDir() }

// HomeDir returns the current user's home directory, or an error. It is
// os.UserHomeDir with the never-guess rule applied and a message that names the
// variable to set.
func HomeDir() (string, error) { return homeDir() }
