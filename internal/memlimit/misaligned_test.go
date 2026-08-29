package memlimit

import (
	"testing"
	"testing/fstest"
)

// A cgroup namespace WITHOUT a cgroupfs remount makes /proc/self/cgroup read
// "0::/" while /sys/fs/cgroup still shows the HOST root — which has no
// memory.max, so a naive reader concludes "unlimited" and reports physical RAM.
// Measured before this branch existed: 15.56 GiB against a real 2 GiB limit,
// the unsafe direction.
//
// Two real signals distinguish it, both verified on this kernel:
//
//  1. The cgroup2 mount's ROOT FIELD in /proc/self/mountinfo. The kernel renders
//     it relative to the READER's cgroup-namespace root, so it is "/" when the
//     mount and /proc/self/cgroup agree, and something else ("/../../..") when
//     they do not.
//  2. When they disagree the host tree is still readable — we simply do not know
//     which node is ours. Searching cgroup.procs for our own PID recovers it
//     exactly, yielding the real limit rather than a fallback.

func TestCgroupMountRoot_ParsesMountinfo(t *testing.T) {
	// Real lines from this host, plus a decoy mount that must not match.
	const mountinfo = `22 96 0:22 / /sys rw,nosuid - sysfs sysfs rw
32 22 0:27 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
41 22 0:35 / /sys/fs/pstore rw - pstore pstore rw
`
	got, err := parseCgroupMountRoot([]byte(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/" {
		t.Errorf("root = %q, want %q", got, "/")
	}
}

func TestCgroupMountRoot_DetectsNamespaceOffset(t *testing.T) {
	// Exactly what `unshare --cgroup` produced on this kernel.
	const mountinfo = "32 22 0:27 /../../.. /sys/fs/cgroup rw,nosuid - cgroup2 cgroup2 rw,nsdelegate\n"
	got, err := parseCgroupMountRoot([]byte(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	if got == "/" {
		t.Errorf("root = %q, want the namespace-offset path — this is the signal that the mount and /proc/self/cgroup disagree", got)
	}
}

// TestDetect_MisalignedMount_RecoversViaProcSearch is the fix for state B: when
// the views disagree, find our own node by PID and read the REAL limit instead
// of falling through to physical RAM.
func TestDetect_MisalignedMount_RecoversViaProcSearch(t *testing.T) {
	const pid = 649093
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("cpuset cpu io memory pids\n")},
		// Host root: no memory.max, which is what fooled the old code.
		"proc/self/cgroup": &fstest.MapFile{Data: []byte("0::/\n")},
		// The mount does NOT correspond to our cgroup view.
		"proc/self/mountinfo": &fstest.MapFile{Data: []byte("32 22 0:27 /../../.. /sys/fs/cgroup rw - cgroup2 cgroup2 rw\n")},
		// Our actual node, discoverable by scanning cgroup.procs.
		"sys/fs/cgroup/system.slice/cgroup.procs":                &fstest.MapFile{Data: []byte("12\n34\n")},
		"sys/fs/cgroup/system.slice/knomit.service/cgroup.procs": &fstest.MapFile{Data: []byte("999\n649093\n1001\n")},
		"sys/fs/cgroup/system.slice/knomit.service/memory.max":   &fstest.MapFile{Data: []byte("2147483648\n")},
	}
	got := detectWithPID(fsys, hostTotal(16*gib), pid)
	if got.Source != SourceCgroupV2 || got.Bytes != 2*gib {
		t.Errorf("got %+v, want 2 GiB from cgroup-v2 — the limit is discoverable, not lost", got)
	}
}

// TestDetect_MisalignedMount_UnknownRatherThanOverReport: if the views disagree
// AND the PID search cannot find us, we know a limit may exist but not what it
// is. Reporting physical RAM would assert "unlimited", which is the unsafe
// direction; reporting unknown is honest and makes the caller fall back
// visibly, with "unavailable" in the boot log for an operator to act on.
func TestDetect_MisalignedMount_UnknownRatherThanOverReport(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":       &fstest.MapFile{Data: []byte("memory\n")},
		"proc/self/cgroup":                       &fstest.MapFile{Data: []byte("0::/\n")},
		"proc/self/mountinfo":                    &fstest.MapFile{Data: []byte("32 22 0:27 /../../.. /sys/fs/cgroup rw - cgroup2 cgroup2 rw\n")},
		"sys/fs/cgroup/other.slice/cgroup.procs": &fstest.MapFile{Data: []byte("1\n2\n")},
	}
	got := detectWithPID(fsys, hostTotal(16*gib), 649093)
	if got.Source == SourceOSTotal {
		t.Errorf("Source = %q — reporting physical RAM here asserts \"unlimited\" when we know the mount is offset", got.Source)
	}
	// SourceUnreadable, NOT SourceNone. The two are different claims and they
	// justify different behaviour downstream: "we found no evidence of a limit"
	// permits normal concurrency, whereas "we know a limit may apply and could
	// not read it" must not — otherwise we run the full default budget with
	// unbounded overlap, which is behaviourally identical to the over-report
	// this branch exists to prevent, with only the log line changed.
	if got.Source != SourceUnreadable {
		t.Errorf("Source = %q, want %q", got.Source, SourceUnreadable)
	}
	if got.Known() {
		t.Error("Known()=true for an unreadable ceiling — there is no usable number here")
	}
}

// TestDetect_AlignedMountIsUnaffected: an ordinary host process at the root
// cgroup has an aligned mount and no limit, and must still fall through to
// physical RAM — that is the correct answer there, and the whole reason state B
// was mistaken for it.
func TestDetect_AlignedMountIsUnaffected(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
		"proc/self/mountinfo":              &fstest.MapFile{Data: []byte("32 22 0:27 / /sys/fs/cgroup rw - cgroup2 cgroup2 rw\n")},
	}
	got := detectWithPID(fsys, hostTotal(16*gib), 1)
	if got.Source != SourceOSTotal || got.Bytes != 16*gib {
		t.Errorf("got %+v, want a clean fall-through to physical RAM", got)
	}
}

// TestDetect_NoMountinfo_AssumesAligned keeps the detector working where
// mountinfo is unreadable: absent evidence of misalignment, behave as before
// rather than degrading every host to "unknown".
func TestDetect_NoMountinfo_AssumesAligned(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/memory.max":         &fstest.MapFile{Data: []byte("2147483648\n")},
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
	}
	got := detectWithPID(fsys, hostTotal(16*gib), 1)
	if got.Bytes != 2*gib || got.Source != SourceCgroupV2 {
		t.Errorf("got %+v, want 2 GiB — a missing mountinfo must not break detection", got)
	}
}

// TestCgroupMountRoot_TakesTheLastMatch: mountinfo lists mounts in order, so
// with an overmount on /sys/fs/cgroup the LATER line is the one actually
// visible. Taking the first reads a shadowed mount's root — and in this order it
// would read "aligned" for a mount that is not ours, walk a path that is not
// ours, find nothing, and fall through to physical RAM: the over-report this
// package exists to prevent, reintroduced from behind.
func TestCgroupMountRoot_TakesTheLastMatch(t *testing.T) {
	const mountinfo = `32 22 0:27 / /sys/fs/cgroup rw shared:9 - cgroup2 cgroup2 rw
88 22 0:31 /../../.. /sys/fs/cgroup rw - cgroup2 cgroup2 rw
`
	got, err := parseCgroupMountRoot([]byte(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/../../.." {
		t.Errorf("root = %q, want the LAST (visible) mount's root", got)
	}
}

// TestCgroupMountRoot_HandlesOptionalFields uses the REAL line from this host,
// optional fields included. The parser's doc comment specifically explains that
// the fields before " - " are variable in number — and every other fixture in
// this package drops them, which is exactly the shape where a fixture stops
// testing the thing the code was written to handle.
func TestCgroupMountRoot_HandlesOptionalFields(t *testing.T) {
	const real = "32 22 0:27 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot\n"
	got, err := parseCgroupMountRoot([]byte(real))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/" {
		t.Errorf("root = %q, want %q", got, "/")
	}
}

// TestFindCgroupByPID_MountRootIsAValidAnswer: "" means our node IS the mount
// root, which is a legitimate result and must not be reported as "not found".
func TestFindCgroupByPID_MountRootIsAValidAnswer(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.procs": &fstest.MapFile{Data: []byte("42\n")},
	}
	got, ok := findCgroupByPID(fsys, 42)
	if !ok {
		t.Error("ok=false for a pid found at the mount root — \"\" is an answer, not a sentinel")
	}
	if got != "" {
		t.Errorf("path = %q, want \"\" (the mount root)", got)
	}
}

// TestCgroupV1_HierarchicalLimitBranch covers the ancestor-accounting field,
// which had no test at all: both v1 fixtures used memory.limit_in_bytes only.
func TestCgroupV1_HierarchicalLimitBranch(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/memory/memory.limit_in_bytes": &fstest.MapFile{Data: []byte("9223372036854771712\n")},
		"sys/fs/cgroup/memory/memory.stat": &fstest.MapFile{
			Data: []byte("cache 0\nrss 123\nhierarchical_memory_limit 4294967296\ntotal_rss 456\n")},
	}
	got := detect(fsys, hostTotal(64*gib))
	if got.Bytes != 4*gib || got.Source != SourceCgroupV1 {
		t.Errorf("got %+v, want 4 GiB from cgroup-v1 via hierarchical_memory_limit", got)
	}
}
