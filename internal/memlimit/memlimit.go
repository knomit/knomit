// Package memlimit reports how much memory this process is actually allowed,
// preferring a cgroup limit over the machine's physical RAM.
//
// It exists because /proc/meminfo lies inside a container: it reports the
// HOST's memory, so anything sized from it hands a 2 GiB container a budget
// meant for a 64 GiB machine. That is how a memory-sized batch budget
// reintroduces the very OOM it was added to prevent.
//
// Nothing here is ever fatal. Embeddings are mandatory — the service refuses
// to start without an embedder — so an undetectable limit must degrade to
// "unknown" and let the caller fall back, never stop startup.
package memlimit

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

// Source names where a reported limit came from. It is logged, so an operator
// debugging an unexpected budget can tell "the container caps us at 2 GiB"
// from "we guessed off physical RAM".
type Source string

const (
	SourceCgroupV2 Source = "cgroup-v2"
	SourceCgroupV1 Source = "cgroup-v1"
	SourceOSTotal  Source = "os-total"
	SourceNone     Source = "unavailable"
)

// Limit is the memory ceiling this process should size itself against.
// Bytes is 0 exactly when Source is SourceNone.
type Limit struct {
	Bytes  int64
	Source Source
	// Inherited reports that the winning limit was found on an ANCESTOR cgroup
	// rather than on our own. It matters because the two mean different things:
	// a limit on our own cgroup was drawn around this workload, while one on a
	// parent slice was drawn around a GROUP we merely belong to, and claiming
	// most of that would be claiming most of someone else's budget.
	//
	// Always false when Source is SourceOSTotal (physical RAM is shared with
	// everything by definition, which callers already treat as unknown
	// co-tenants) or SourceNone.
	Inherited bool
	// HostTotal is physical RAM as reported by the OS, 0 when unknown. Kept
	// alongside Bytes because they answer different questions: Bytes is what we
	// may not exceed, HostTotal is how much slack exists outside our claim to
	// absorb a mistake.
	HostTotal int64
}

// Known reports whether a usable limit was found.
func (l Limit) Known() bool { return l.Source != SourceNone && l.Bytes > 0 }

var errNotFound = errors.New("memlimit: no value")

// errMisaligned means the cgroup2 mount does not correspond to our own cgroup
// view AND we could not recover our node. It is deliberately NOT errNotFound:
// "no limit found" may fall through to physical RAM, but "a limit may exist and
// we cannot read it" must not — reporting physical RAM there asserts unlimited,
// which is the unsafe direction.
var errMisaligned = errors.New("memlimit: cgroup mount does not match our cgroup view")

// v1Unlimited: cgroup v1 encodes "no limit" as PAGE_COUNTER_MAX rather than a
// word, so an absurdly large positive number means unset. Anything at or above
// 2^60 is treated as unset — no real machine has an exbibyte of RAM, and the
// exact sentinel varies with page size across kernels.
const v1Unlimited = int64(1) << 60

// Detect reports this process's memory ceiling, reading the real filesystem.
func Detect() Limit { return detectWithPID(os.DirFS("/"), osTotal, os.Getpid()) }

// detect is Detect with its two inputs injected, so every branch — including
// the container-only ones — is testable on a machine that has none of them.
//
// Precedence is cgroup v2, then cgroup v1, then physical RAM; the result is
// then ALWAYS min'd with physical RAM, because a stale or nonsensical cgroup
// limit above the machine's memory is an upper bound the machine cannot honour.
// Any unreadable or malformed source is skipped rather than reported.
func detect(fsys fs.FS, total func() (int64, error)) Limit {
	return detectWithPID(fsys, total, os.Getpid())
}

// detectWithPID is detect with the process id injected, which the misaligned-
// mount recovery needs (it identifies our cgroup by finding our own pid in a
// cgroup.procs file).
func detectWithPID(fsys fs.FS, total func() (int64, error), pid int) Limit {
	hostTotal, totalErr := total()

	clamp := func(n int64, src Source, inherited bool) Limit {
		if totalErr == nil && hostTotal > 0 && n > hostTotal {
			n = hostTotal
		}
		l := Limit{Bytes: n, Source: src, Inherited: inherited}
		if totalErr == nil {
			l.HostTotal = hostTotal
		}
		return l
	}

	switch n, inherited, err := cgroupV2Limit(fsys, pid); {
	case err == nil:
		return clamp(n, SourceCgroupV2, inherited)
	case errors.Is(err, errMisaligned):
		// We know the mount is offset from our own cgroup, so a missing
		// memory.max is NOT evidence of "unlimited" and physical RAM would be an
		// over-report. Report unknown instead: the caller falls back to a fixed
		// default and logs source=unavailable, which an operator can act on by
		// configuring an explicit budget.
		return Limit{Source: SourceNone}
	}
	if n, err := cgroupV1Limit(fsys); err == nil {
		// v1 reads the mount root, which in a container IS our own cgroup
		// directory (the runtime bind-mounts it there) — see cgroupV1Limit.
		return clamp(n, SourceCgroupV1, false)
	}
	if totalErr == nil && hostTotal > 0 {
		return Limit{Bytes: hostTotal, Source: SourceOSTotal, HostTotal: hostTotal}
	}
	return Limit{Source: SourceNone}
}

// cgroupV2Limit reads memory.max for this process's cgroup and every ancestor,
// returning the smallest real limit found.
//
// Both halves are load-bearing, and both are verified against the real kernel
// rather than only against fixtures (see memlimit_container_test.go):
//
//   - Inside a container, /proc/self/cgroup reads "0::/" AND the runtime mounts
//     the container's own cgroup at /sys/fs/cgroup, so the limit is at the mount
//     root. Reproduced with cgroup+mount namespaces and a fresh cgroup2 mount
//     under a 2 GiB MemoryMax: detected correctly.
//   - On a non-namespaced host the path is a real hierarchy whose LEAF usually
//     reads "max" while an ancestor slice holds the limit, so reading only the
//     leaf reports unlimited and skips the clamp entirely. Reproduced with
//     systemd-run --scope -p MemoryMax=2G: detected correctly.
//
// A third shape used to defeat this and no longer does. A cgroup namespace
// WITHOUT a cgroupfs remount reports "0::/" while /sys/fs/cgroup still shows the
// HOST root, which carries no memory.max — so the walk found nothing and we fell
// through to physical RAM, over-reporting 15.56 GiB against a real 2 GiB limit.
//
// That state is NOT indistinguishable from an unlimited host process, though it
// looks it from /proc/self/cgroup alone. Two signals separate them, both
// verified on this kernel: the cgroup2 mount's root field in
// /proc/self/mountinfo is "/" only when the mount matches our cgroup view, and
// when it does not, our own pid still appears in exactly one cgroup.procs. So we
// recover the real node and read the real limit. All three shapes now detect
// 2.00 GiB under a MemoryMax=2G scope, while an genuinely unlimited host still
// falls through to physical RAM.
// Returns the limit, whether it came from an ancestor rather than our own
// cgroup, and an error.
func cgroupV2Limit(fsys fs.FS, pid int) (limit int64, inherited bool, err error) {
	// The unified hierarchy is identified by this file; without it the mount is
	// v1 (or absent) and memory.max would not mean what we think.
	if _, err := fs.Stat(fsys, "sys/fs/cgroup/cgroup.controllers"); err != nil {
		return 0, false, errNotFound
	}

	rel, err := ownCgroupV2Path(fsys)
	if err != nil {
		return 0, false, err
	}

	// Does the mount actually correspond to the path we just read? The kernel
	// renders a cgroup2 mount's root RELATIVE TO THE READER's cgroup-namespace
	// root, so this is "/" when the two agree. When it is not — a cgroup
	// namespace without a cgroupfs remount, or a runtime that bind-mounts a
	// subtree — walking from `rel` reads the wrong nodes and typically finds
	// nothing, which the old code mistook for "unlimited".
	if root, err := cgroupMountRoot(fsys); err == nil && root != "/" {
		// The tree is still fully readable; we just do not know which node is
		// ours. Our own pid appears in exactly one cgroup.procs, so find it.
		// cgroup.procs renders pids in the READER's pid namespace, so this also
		// works when the pid namespace is unshared.
		found, ok := findCgroupByPID(fsys, pid)
		if !ok {
			return 0, false, errMisaligned
		}
		rel = found
	}

	best := int64(0)
	fromAncestor := false
	leaf := path.Join("sys/fs/cgroup", rel)
	for dir := leaf; ; dir = path.Dir(dir) {
		if n, err := readInt(fsys, path.Join(dir, "memory.max")); err == nil && n > 0 {
			if best == 0 || n < best {
				best = n
				fromAncestor = dir != leaf
			}
		}
		if dir == "sys/fs/cgroup" || dir == "." || dir == "/" {
			break
		}
	}
	if best == 0 {
		return 0, false, errNotFound
	}
	return best, fromAncestor, nil
}

// ownCgroupV2Path returns this process's cgroup path from the "0::<path>" line
// of /proc/self/cgroup, as a path relative to the cgroup mount.
func ownCgroupV2Path(fsys fs.FS) (string, error) {
	data, err := fs.ReadFile(fsys, "proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return strings.TrimPrefix(after, "/"), nil
		}
	}
	return "", errNotFound
}

// cgroupV1Limit reads the v1 memory controller AT THE MOUNT ROOT
// (/sys/fs/cgroup/memory/). Be precise about why that is usually enough, because
// the obvious reading is wrong: it is NOT that hierarchical_memory_limit saves us
// a walk. This function never consults /proc/self/cgroup at all, so on a v1 HOST
// a process inside a limited cgroup detects nothing here and falls through to
// physical RAM — the safe direction, but a real gap. It works in CONTAINERS
// because the runtime bind-mounts the container's own cgroup directory at that
// path, so the "root" already is our cgroup.
//
// Within whatever directory we do read, hierarchical_memory_limit accounts for
// ancestor limits and is preferred over memory.limit_in_bytes when smaller.
//
// DEFERRED, with a trigger condition rather than left as an oversight: giving v1
// the same treatment v2 has (read /proc/self/cgroup, walk the hierarchy) is not
// done, because the exposure needs a NON-containerized host process under a v1
// LEAF limit — a legacy shape, since v2 has been the default since roughly 2021
// and v1 containers work via the bind-mount above. The failure is also in the
// safe direction: such a host sizes from physical RAM, which over-budgets the
// batch rather than under-detecting a wall. Revisit if a v1 host deployment
// ever actually exists.
func cgroupV1Limit(fsys fs.FS) (int64, error) {
	best := int64(0)
	if n, err := readInt(fsys, "sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil && n > 0 && n < v1Unlimited {
		best = n
	}
	if n, err := readStatField(fsys, "sys/fs/cgroup/memory/memory.stat", "hierarchical_memory_limit"); err == nil && n > 0 && n < v1Unlimited {
		if best == 0 || n < best {
			best = n
		}
	}
	if best == 0 {
		return 0, errNotFound
	}
	return best, nil
}

func readInt(fsys fs.FS, name string) (int64, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "max" { // v2's "no limit"
		return 0, errNotFound
	}
	return strconv.ParseInt(s, 10, 64)
}

func readStatField(fsys fs.FS, name, field string) (int64, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && k == field {
			return strconv.ParseInt(v, 10, 64)
		}
	}
	return 0, errNotFound
}

// parseMemTotal pulls MemTotal (reported in kB) out of /proc/meminfo.
func parseMemTotal(data []byte) (int64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, errNotFound
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, errNotFound
}

// cgroupMountRoot returns the root field of the cgroup2 mount at
// /sys/fs/cgroup: "/" when the mount matches our cgroup-namespace view.
func cgroupMountRoot(fsys fs.FS) (string, error) {
	data, err := fs.ReadFile(fsys, "proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	return parseCgroupMountRoot(data)
}

// parseCgroupMountRoot picks the cgroup2 mount at /sys/fs/cgroup out of
// mountinfo and returns its root field.
//
// mountinfo format: id parent major:minor ROOT MOUNTPOINT opts... - FSTYPE ...
// The optional fields before " - " are variable in number, so the filesystem
// type must be read AFTER the separator, not at a fixed index.
func parseCgroupMountRoot(data []byte) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		f := strings.Fields(before)
		if len(f) < 5 || f[4] != "/sys/fs/cgroup" {
			continue
		}
		post := strings.Fields(after)
		if len(post) == 0 || post[0] != "cgroup2" {
			continue
		}
		return f[3], nil
	}
	return "", errNotFound
}

// findCgroupByPID locates the cgroup node containing pid by scanning
// cgroup.procs across the hierarchy, returning a path relative to the mount.
//
// Only reached when the mount and our cgroup view disagree — it is a directory
// walk (~7ms over ~68 files on a typical host), not something to do on every
// boot when the cheap path works.
func findCgroupByPID(fsys fs.FS, pid int) (string, bool) {
	want := strconv.Itoa(pid)
	found := ""
	_ = fs.WalkDir(fsys, "sys/fs/cgroup", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "cgroup.procs" || found != "" {
			return nil //nolint:nilerr // an unreadable subtree is skipped, not fatal
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == want {
				found = strings.TrimPrefix(path.Dir(p), "sys/fs/cgroup")
				found = strings.TrimPrefix(found, "/")
				return fs.SkipAll
			}
		}
		return nil
	})
	return found, found != ""
}
