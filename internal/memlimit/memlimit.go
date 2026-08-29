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
}

// Known reports whether a usable limit was found.
func (l Limit) Known() bool { return l.Source != SourceNone && l.Bytes > 0 }

var errNotFound = errors.New("memlimit: no value")

// v1Unlimited: cgroup v1 encodes "no limit" as PAGE_COUNTER_MAX rather than a
// word, so an absurdly large positive number means unset. Anything at or above
// 2^60 is treated as unset — no real machine has an exbibyte of RAM, and the
// exact sentinel varies with page size across kernels.
const v1Unlimited = int64(1) << 60

// Detect reports this process's memory ceiling, reading the real filesystem.
func Detect() Limit { return detect(os.DirFS("/"), osTotal) }

// detect is Detect with its two inputs injected, so every branch — including
// the container-only ones — is testable on a machine that has none of them.
//
// Precedence is cgroup v2, then cgroup v1, then physical RAM; the result is
// then ALWAYS min'd with physical RAM, because a stale or nonsensical cgroup
// limit above the machine's memory is an upper bound the machine cannot honour.
// Any unreadable or malformed source is skipped rather than reported.
func detect(fsys fs.FS, total func() (int64, error)) Limit {
	hostTotal, totalErr := total()

	clamp := func(n int64, src Source) Limit {
		if totalErr == nil && hostTotal > 0 && n > hostTotal {
			n = hostTotal
		}
		return Limit{Bytes: n, Source: src}
	}

	if n, err := cgroupV2Limit(fsys); err == nil {
		return clamp(n, SourceCgroupV2)
	}
	if n, err := cgroupV1Limit(fsys); err == nil {
		return clamp(n, SourceCgroupV1)
	}
	if totalErr == nil && hostTotal > 0 {
		return Limit{Bytes: hostTotal, Source: SourceOSTotal}
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
// KNOWN LIMITATION, in the unsafe direction. A cgroup namespace WITHOUT a
// cgroupfs remount (e.g. bare `unshare --cgroup`) reports "0::/" while
// /sys/fs/cgroup still shows the HOST root, which carries no memory.max. We
// then fall through to physical RAM and over-report — measured 15.56 GiB
// against a real 2 GiB limit. This is not fixable from inside: that state is
// indistinguishable from an ordinary unlimited host process at the root cgroup,
// where falling through to physical RAM is exactly right. Real container
// runtimes do remount, so this affects unusual sandboxes rather than Docker.
func cgroupV2Limit(fsys fs.FS) (int64, error) {
	// The unified hierarchy is identified by this file; without it the mount is
	// v1 (or absent) and memory.max would not mean what we think.
	if _, err := fs.Stat(fsys, "sys/fs/cgroup/cgroup.controllers"); err != nil {
		return 0, errNotFound
	}

	rel, err := ownCgroupV2Path(fsys)
	if err != nil {
		return 0, err
	}

	best := int64(0)
	for dir := path.Join("sys/fs/cgroup", rel); ; dir = path.Dir(dir) {
		if n, err := readInt(fsys, path.Join(dir, "memory.max")); err == nil && n > 0 {
			if best == 0 || n < best {
				best = n
			}
		}
		if dir == "sys/fs/cgroup" || dir == "." || dir == "/" {
			break
		}
	}
	if best == 0 {
		return 0, errNotFound
	}
	return best, nil
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

// cgroupV1Limit reads the v1 memory controller. hierarchical_memory_limit in
// memory.stat already accounts for ancestor limits, so v1 needs no walk; it is
// preferred over memory.limit_in_bytes when present and smaller.
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
