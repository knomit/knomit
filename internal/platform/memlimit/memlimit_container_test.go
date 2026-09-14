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

// The Inherited bit swings the budget by up to 3.2x downstream, and until now
// nothing drove it through detect() — every test of it hand-constructed a Limit
// in another package. These pin it against the walk that actually sets it.

func TestDetect_InheritedFalse_WhenTheLimitIsOnOurOwnCgroup(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":                     &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/system.slice/memory.max":                &fstest.MapFile{Data: []byte("max\n")},
		"sys/fs/cgroup/system.slice/knomit.service/memory.max": &fstest.MapFile{Data: []byte("2147483648\n")},
		"proc/self/cgroup":                                     &fstest.MapFile{Data: []byte("0::/system.slice/knomit.service\n")},
	}
	got := detect(fsys, hostTotal(64*gib))
	if got.Inherited {
		t.Error("Inherited=true for a limit on our own leaf cgroup — this is the dedicated case")
	}
}

func TestDetect_InheritedTrue_WhenAnAncestorIsSmaller(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":                     &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/system.slice/memory.max":                &fstest.MapFile{Data: []byte("2147483648\n")},
		"sys/fs/cgroup/system.slice/knomit.service/memory.max": &fstest.MapFile{Data: []byte("8589934592\n")},
		"proc/self/cgroup":                                     &fstest.MapFile{Data: []byte("0::/system.slice/knomit.service\n")},
	}
	got := detect(fsys, hostTotal(64*gib))
	if got.Bytes != 2*gib {
		t.Errorf("Bytes = %d, want the smaller ANCESTOR limit", got.Bytes)
	}
	if !got.Inherited {
		t.Error("Inherited=false although the winning limit came from a shared parent slice")
	}
}

// TestDetect_InheritedFalse_OnATie: leaf and ancestor carry the same limit. The
// walk visits the leaf first and `n < best` is strict, so the leaf keeps it —
// the right call, since a limit that is also present on our own cgroup is
// plausibly ours.
func TestDetect_InheritedFalse_OnATie(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":                     &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/system.slice/memory.max":                &fstest.MapFile{Data: []byte("2147483648\n")},
		"sys/fs/cgroup/system.slice/knomit.service/memory.max": &fstest.MapFile{Data: []byte("2147483648\n")},
		"proc/self/cgroup":                                     &fstest.MapFile{Data: []byte("0::/system.slice/knomit.service\n")},
	}
	if got := detect(fsys, hostTotal(64*gib)); got.Inherited {
		t.Error("Inherited=true on a tie — the leaf should win")
	}
}

// TestDetect_ClampBindingMarksTheLimitShared is H4: a cgroup limit ABOVE
// physical RAM (generous k8s/systemd template limits are ordinary) clamps down
// to the machine — at which point the effective ceiling is physical RAM, shared
// with everything, and treating it as dedicated claims 0.8 of a box we do not
// own. Measured before the fix: 14203 tokens vs 2048 for the same 4 GiB box
// seen as physical RAM, a 6.9x swing.
func TestDetect_ClampBindingMarksTheLimitShared(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/memory.max":         &fstest.MapFile{Data: []byte("34359738368\n")}, // 32 GiB
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
	}
	got := detect(fsys, hostTotal(4*gib))
	if got.Bytes != 4*gib {
		t.Errorf("Bytes = %d, want the clamp to physical RAM", got.Bytes)
	}
	if !got.Inherited {
		t.Error("Inherited=false after the clamp bound — once the effective ceiling IS physical RAM, the limit is not dedicated to us")
	}
}
