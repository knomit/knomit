package memlimit

import (
	"testing"
	"testing/fstest"
)

// The detector reads a filesystem rather than hardcoding /sys and /proc paths,
// so every branch below — including the ones that only occur inside a
// container — is exercisable on a developer machine that has none of them.

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

// hostTotal stands in for the OS-reported physical memory.
func hostTotal(n int64) func() (int64, error) {
	return func() (int64, error) { return n, nil }
}

func hostTotalFails() (int64, error) { return 0, errNotFound }

// TestDetect_CgroupV2_ContainerNamespace covers the case that matters most:
// inside a container with a private cgroup namespace (docker's default on v2)
// /proc/self/cgroup reads "0::/" and the limit sits at the root of the mount.
// Getting this wrong reads the HOST's memory from /proc/meminfo and hands a
// 2 GiB container a budget sized for a 64 GiB machine — the exact failure the
// whole clamp-down exists to prevent.
func TestDetect_CgroupV2_ContainerNamespace(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("memory pids\n")},
		"sys/fs/cgroup/memory.max":         &fstest.MapFile{Data: []byte("2147483648\n")},
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
	}
	got := detect(fsys, hostTotal(64*gib))
	if got.Bytes != 2*gib {
		t.Errorf("Bytes = %d, want %d (the container limit, not the host's RAM)", got.Bytes, 2*gib)
	}
	if got.Source != SourceCgroupV2 {
		t.Errorf("Source = %q, want %q", got.Source, SourceCgroupV2)
	}
}

// TestDetect_CgroupV2_WalksAncestors covers the non-namespaced host process:
// the leaf cgroup usually says "max" while a parent slice carries the real
// limit. Reading only the leaf reports unlimited and silently skips the clamp.
func TestDetect_CgroupV2_WalksAncestors(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":                     &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/memory.max":                             &fstest.MapFile{Data: []byte("max\n")},
		"sys/fs/cgroup/system.slice/memory.max":                &fstest.MapFile{Data: []byte("8589934592\n")},
		"sys/fs/cgroup/system.slice/knomit.service/memory.max": &fstest.MapFile{Data: []byte("max\n")},
		"proc/self/cgroup":                                     &fstest.MapFile{Data: []byte("0::/system.slice/knomit.service\n")},
	}
	got := detect(fsys, hostTotal(64*gib))
	if got.Bytes != 8*gib {
		t.Errorf("Bytes = %d, want %d (the parent slice's limit; the leaf says max)", got.Bytes, 8*gib)
	}
	if got.Source != SourceCgroupV2 {
		t.Errorf("Source = %q, want %q", got.Source, SourceCgroupV2)
	}
}

// TestDetect_CgroupV2_AllMaxFallsThrough is this development machine's own
// shape: cgroup v2 present, every level unlimited. "max" is not a limit, so
// detection must fall through to physical memory rather than report success.
func TestDetect_CgroupV2_AllMaxFallsThrough(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers":    &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/memory.max":            &fstest.MapFile{Data: []byte("max\n")},
		"sys/fs/cgroup/user.slice/memory.max": &fstest.MapFile{Data: []byte("max\n")},
		"proc/self/cgroup":                    &fstest.MapFile{Data: []byte("0::/user.slice\n")},
		"proc/meminfo":                        &fstest.MapFile{Data: []byte("MemTotal:       16317892 kB\n")},
	}
	got := detect(fsys, hostTotal(16*gib))
	if got.Source != SourceOSTotal {
		t.Errorf("Source = %q, want %q — \"max\" at every level is not a limit", got.Source, SourceOSTotal)
	}
	if got.Bytes != 16*gib {
		t.Errorf("Bytes = %d, want %d", got.Bytes, 16*gib)
	}
}

// TestDetect_CgroupV1 reads the v1 layout, used when the unified mount marker
// is absent.
func TestDetect_CgroupV1(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/memory/memory.limit_in_bytes": &fstest.MapFile{Data: []byte("4294967296\n")},
	}
	got := detect(fsys, hostTotal(64*gib))
	if got.Bytes != 4*gib {
		t.Errorf("Bytes = %d, want %d", got.Bytes, 4*gib)
	}
	if got.Source != SourceCgroupV1 {
		t.Errorf("Source = %q, want %q", got.Source, SourceCgroupV1)
	}
}

// TestDetect_CgroupV1_UnlimitedSentinel guards the v1 "no limit" encoding,
// which is not a word but PAGE_COUNTER_MAX — a huge positive number. Treating
// it as a real limit would report ~8 EiB and defeat the min() with host RAM.
func TestDetect_CgroupV1_UnlimitedSentinel(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/memory/memory.limit_in_bytes": &fstest.MapFile{Data: []byte("9223372036854771712\n")},
	}
	got := detect(fsys, hostTotal(16*gib))
	if got.Source != SourceOSTotal {
		t.Errorf("Source = %q, want %q — PAGE_COUNTER_MAX means unset", got.Source, SourceOSTotal)
	}
}

// TestDetect_AlwaysMinsWithHostTotal guards a stale or nonsensical cgroup limit
// larger than the machine's RAM. The limit is an upper bound on what the
// machine can actually give us, never a promise.
func TestDetect_AlwaysMinsWithHostTotal(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/memory.max":         &fstest.MapFile{Data: []byte("137438953472\n")}, // 128 GiB
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
	}
	got := detect(fsys, hostTotal(16*gib))
	if got.Bytes != 16*gib {
		t.Errorf("Bytes = %d, want %d — a limit above physical RAM must be clamped", got.Bytes, 16*gib)
	}
}

// TestDetect_NoSourcesAvailable is the darwin/windows/strange-sandbox path.
// Detection must report unavailable rather than guessing or failing: embeddings
// are mandatory (the service refuses to boot without an embedder), so an
// undetectable memory limit must never be able to stop startup.
func TestDetect_NoSourcesAvailable(t *testing.T) {
	got := detect(fstest.MapFS{}, hostTotalFails)
	if got.Source != SourceNone {
		t.Errorf("Source = %q, want %q", got.Source, SourceNone)
	}
	if got.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0 when nothing is known", got.Bytes)
	}
}

// TestDetect_UnreadableCgroupFallsThrough pins the "never fatal" contract: a
// malformed or unreadable source is skipped, not surfaced as an error.
func TestDetect_UnreadableCgroupFallsThrough(t *testing.T) {
	fsys := fstest.MapFS{
		"sys/fs/cgroup/cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"sys/fs/cgroup/memory.max":         &fstest.MapFile{Data: []byte("not-a-number\n")},
		"proc/self/cgroup":                 &fstest.MapFile{Data: []byte("0::/\n")},
	}
	got := detect(fsys, hostTotal(16*gib))
	if got.Source != SourceOSTotal || got.Bytes != 16*gib {
		t.Errorf("got %+v, want a clean fall-through to host total", got)
	}
}

func TestParseMemTotal(t *testing.T) {
	const meminfo = "MemFree:        13577696 kB\nMemTotal:       16317892 kB\nBuffers:           17268 kB\n"
	got, err := parseMemTotal([]byte(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(16317892) * 1024; got != want {
		t.Errorf("parseMemTotal = %d, want %d", got, want)
	}
}

// TestDetect_OnThisHost exercises the real filesystem. It cannot assert a
// specific number — that depends on the machine — but it pins the invariants
// that must hold everywhere: detection never panics, Bytes and Source always
// agree, and a known limit never exceeds physical memory.
func TestDetect_OnThisHost(t *testing.T) {
	l := Detect()
	t.Logf("detected on this host: source=%s bytes=%d (%.2f GiB)", l.Source, l.Bytes, float64(l.Bytes)/float64(gib))

	if l.Known() != (l.Bytes > 0) {
		t.Errorf("Known()=%v but Bytes=%d — the two must agree", l.Known(), l.Bytes)
	}
	if l.Source == SourceNone && l.Bytes != 0 {
		t.Errorf("SourceNone must carry Bytes=0, got %d", l.Bytes)
	}
	if total, err := osTotal(); err == nil && l.Known() && l.Bytes > total {
		t.Errorf("detected %d > physical %d — the min() with host total did not apply", l.Bytes, total)
	}
}
