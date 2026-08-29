package memlimit

import (
	"testing"
	"testing/fstest"
)

// These pin the two cgroup shapes that were reproduced against the real kernel,
// so the fixtures stay honest. Both were verified on 2026-08-29 under a real
// `systemd-run --scope -p MemoryMax=2G`:
//
//	host process                                    -> cgroup-v2, 2.00 GiB  (correct)
//	+ cgroup ns + mount ns + fresh cgroup2 mount    -> cgroup-v2, 2.00 GiB  (correct)
//	+ cgroup ns only, no remount                    -> os-total, 15.56 GiB  (over-reports)
//
// The fixtures below encode the kernel's ACTUAL output in each case, not my
// reading of the documentation — which matters, because a fixture written from
// a wrong mental model and an implementation written from the same wrong model
// agree with each other and the test passes anyway.

// TestDetect_RealContainerShape uses the exact strings the kernel produced
// inside a cgroup+mount namespace with cgroup2 freshly mounted: the path
// collapses to "0::/" and the container's own limit sits at the mount root.
func TestDetect_RealContainerShape(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("cpuset cpu io memory pids\n")},
		"sys/fs/cgroup/memory.max":         &fstest.MapFile{Data: []byte("2147483648\n")},
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
	}
	got := detect(fsys, hostTotal(16*gib))
	if got.Bytes != 2*gib || got.Source != SourceCgroupV2 {
		t.Errorf("got %+v, want 2 GiB from cgroup-v2", got)
	}
}

// TestDetect_RealHostScopeShape uses the path systemd actually produced for a
// transient scope. Note the leaf carries the limit here, while its ancestors do
// not — the walk must take the minimum of what it finds, not the first value.
func TestDetect_RealHostScopeShape(t *testing.T) {
	const scope = "sys/fs/cgroup/system.slice/run-r3484e62297d94ca09f4f3db966eefed2.scope"
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":      &fstest.MapFile{Data: []byte("cpuset cpu io memory pids\n")},
		"sys/fs/cgroup/memory.max":              &fstest.MapFile{Data: []byte("max\n")},
		"sys/fs/cgroup/system.slice/memory.max": &fstest.MapFile{Data: []byte("max\n")},
		scope + "/memory.max":                   &fstest.MapFile{Data: []byte("2147483648\n")},
		"proc/self/cgroup":                      &fstest.MapFile{Data: []byte("0::/system.slice/run-r3484e62297d94ca09f4f3db966eefed2.scope\n")},
	}
	got := detect(fsys, hostTotal(16*gib))
	if got.Bytes != 2*gib || got.Source != SourceCgroupV2 {
		t.Errorf("got %+v, want 2 GiB from cgroup-v2", got)
	}
}
