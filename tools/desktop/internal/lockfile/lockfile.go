// Package lockfile is the desktop's name for knomit/internal/lockfile, which
// moved there so `knomit serve` can write the same file. It is an alias and
// nothing more; the separate-process work removes its last importers.
package lockfile

import l "knomit/internal/lockfile"

type Info = l.Info

var ErrCorrupt = l.ErrCorrupt

func Write(path string, info Info) error { return l.Write(path, info) }
func Read(path string) (Info, error)     { return l.Read(path) }
func Remove(path string) error           { return l.Remove(path) }
