package kb

import (
	"knomit/internal/corpus/federate"
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
// It is federate.FanOut — the one concurrent lens fan-out, shared with the
// fused search behind /lenses/{lens}/search and MCP knomit_query — plus the
// problem TITLE each leg reports. The rules it keeps (index-confined writes,
// lowest index wins, a panic becomes the mount's error, a mount label that
// cannot itself panic) are documented on federate.FanOut and in fact b9c7ab96.
//
// fn returns the problem TITLE alongside its error because one leg can fail in
// more than one way (stats fetches both aggregates and activity) and the titles
// differ; carrying it here keeps the reported title the one the serial code
// reported for that same failure. A recovered panic reports "Mount failed".
func fanOutMounts(targets []federate.Target, fn func(i int, t federate.Target) (string, error)) *mountFailure {
	titles := make([]string, len(targets))
	f := federate.FanOut(targets, func(i int, t federate.Target) error {
		title, err := fn(i, t)
		titles[i] = title
		return err
	})
	if f == nil {
		return nil
	}
	title := titles[f.Mount]
	if f.Panicked {
		title = "Mount failed"
	}
	return &mountFailure{Mount: f.Mount, Title: title, Err: f.Err}
}
