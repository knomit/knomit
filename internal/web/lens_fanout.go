package web

import (
	"fmt"
	"sync"

	"knomit/internal/federate"
)

// mountFailure is one mount's failed fan-out leg: which mount, the error, and
// the problem title the handler would have reported had the loop been serial.
type mountFailure struct {
	Mount int
	Title string
	Err   error
}

// fanOutMounts runs fn against every mount CONCURRENTLY and returns the failure
// of the LOWEST-INDEXED mount that failed, or nil when all succeeded.
//
// Why concurrently: a lens read's cost was the SUM of its mounts, and mounts are
// independent SQLite databases with nothing to serialise on — RepoInstance
// acquisition is an RWMutex read lock plus a WaitGroup, never exclusive. Serial
// was simply the shape the single-mount original had. internal/mcp/query.go's
// fan-outs have always been parallel; this is the same pattern, kept
// deliberately identical rather than re-invented.
//
// Why lowest-indexed and not first-to-fail: RFC §9.1 says any mount error fails
// the whole request — a lens must never silently shrink its read set — and that
// was already true serially. What parallelism could quietly change is WHICH
// error a caller sees when two mounts fail, and the wording of a lens error
// names the offending mount's branch. Racing goroutines would make that a
// coin-flip between runs. Taking the lowest index yields exactly the error the
// serial loop would have hit first, so the response is a function of the request
// and not of scheduling.
//
// fn MUST confine its writes to its own index. Every caller collects into
// slices pre-sized to len(targets) and reduces them afterwards, in index order,
// on the request goroutine — never appending from inside a goroutine, which
// would make the response order depend on which mount finished first.
//
// fn returns the problem TITLE alongside its error because one leg can fail in
// more than one way (stats fetches both aggregates and activity) and the titles
// differ; carrying it here keeps the reported title the one the serial code
// reported for that same failure.
func fanOutMounts(targets []federate.Target, fn func(i int, t federate.Target) (string, error)) *mountFailure {
	failures := make([]*mountFailure, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		// The mount's label is built HERE, before the goroutine, and without
		// dereferencing anything that might be absent.
		//
		// It is the recover handler's message, and a recover handler that can
		// itself panic is worse than none: the second panic escapes the
		// goroutine and kills the process — precisely what the recover exists
		// to prevent. RepoInstance.Name() reads a field, so it panics on a nil
		// instance, and a nil RepoInstance is the same archive/shutdown-race
		// family that motivates the recover in the first place. Naming the
		// mount must never be the thing that fails.
		label := "mount " + t.RT.Branch
		if t.RT.RI != nil {
			label = "mount " + t.RT.RI.Name() + "@" + t.RT.Branch
		}
		wg.Add(1)
		go func(i int, t federate.Target, label string) {
			defer wg.Done()
			// A panic here must become this mount's error, not the process's
			// death. net/http recovers panics on the REQUEST goroutine only, so
			// an unrecovered panic in one of these bare per-mount goroutines
			// takes the whole server down rather than the one connection —
			// exactly the widening internal/mcp/query.go's recoverFanout exists
			// to prevent, and a real one: an archive/shutdown race can leave a
			// mount's svc nil. Routed into the mount's slot, it flows out
			// through the ordinary §9.1 path.
			defer func() {
				if p := recover(); p != nil {
					failures[i] = &mountFailure{Mount: i, Title: "Mount failed",
						Err: fmt.Errorf("%s panicked: %v", label, p)}
				}
			}()
			if title, err := fn(i, t); err != nil {
				failures[i] = &mountFailure{Mount: i, Title: title, Err: err}
			}
		}(i, t, label)
	}
	wg.Wait()
	for _, f := range failures {
		if f != nil {
			return f
		}
	}
	return nil
}
