package embeddings

import (
	"testing"

	"knomit/internal/memlimit"
)

const gib = int64(1) << 30

func cgroupLimit(mib int64, inherited bool) memlimit.Limit {
	return memlimit.Limit{
		Bytes: mib << 20, Source: memlimit.SourceCgroupV2,
		Inherited: inherited, HostTotal: 64 * gib,
	}
}

func hostLimit(mib int64) memlimit.Limit {
	return memlimit.Limit{Bytes: mib << 20, Source: memlimit.SourceOSTotal, HostTotal: mib << 20}
}

// ── The serialization rule keys on the DEATH BOUNDARY, not on whether we
// clamped. The previous rule ("did the budget come in below the ceiling")
// contained a live OOM window: a 4400 MiB cgroup budgeted for exactly one run
// with 1 MiB of slack and then disabled the gate that made "one run" true, so
// adding 100 MiB to a 4300 MiB container REMOVED the per-process bound.

func TestSerialize_CgroupAlwaysSerializes(t *testing.T) {
	// A cgroup is a hard wall: breaching it is instant death with no absorber.
	// Any K-run threshold is beatable by K+1 concurrent branches, so a threshold
	// on the hard path only moves the cliff rather than removing it.
	for _, mib := range []int64{2048, 4300, 4400, 5120, 8192, 65536} {
		got := ResolveBudget(0, cgroupLimit(mib, false))
		if !got.Serialize {
			t.Errorf("cgroup %d MiB: Serialize=false — a hard limit must get a hard guarantee", mib)
		}
	}
}

func TestSerialize_CgroupWithExplicitBudgetStillSerializes(t *testing.T) {
	// S6: an explicit value sets the BUDGET; it is not a concurrency policy, and
	// there is no other way to obtain the memory guarantee. An operator pinning
	// a budget inside a 2 GiB container must not silently lose it.
	got := ResolveBudget(4096, cgroupLimit(2048, false))
	if got.Tokens != 4096 {
		t.Errorf("Tokens = %d, want the configured 4096", got.Tokens)
	}
	if !got.Serialize {
		t.Error("Serialize=false for an explicit budget under a cgroup — serialization derives from the ceiling, not from where Tokens came from")
	}
}

func TestSerialize_RoomyHostKeepsConcurrency(t *testing.T) {
	// The incident machine. Its real safety never came from the gate: it came
	// from the unclaimed ~75% of physical RAM being a genuine absorber. So the
	// check uses the PHYSICAL envelope while the fraction only sizes the budget.
	got := ResolveBudget(0, hostLimit(15900))
	if got.Serialize {
		t.Error("Serialize=true on the deployed 15.9 GiB host — this would be a behaviour change on a machine we confirmed healthy")
	}
}

func TestSerialize_TinyHostSerializes(t *testing.T) {
	// A 2 GiB laptop has no absorber left once the model and knomit's own
	// footprint are accounted for.
	got := ResolveBudget(0, hostLimit(2048))
	if !got.Serialize {
		t.Error("Serialize=false on a 2 GiB host — two overlapping runs do not fit in what remains")
	}
}

func TestSerialize_UnknownCeilingDoesNotSerialize(t *testing.T) {
	// We measured nothing, so we know nothing. An unmeasured host is not a
	// constrained one, and imposing the throughput cost on it is unfounded.
	got := ResolveBudget(0, memlimit.Limit{Source: memlimit.SourceNone})
	if got.Serialize {
		t.Error("Serialize=true on an undetectable ceiling")
	}
	if got.Tokens != DefaultMaxBatchTokens {
		t.Errorf("Tokens = %d, want the fixed default", got.Tokens)
	}
}

// TestSerialize_MonotonicInMemory is the property the old rule violated: adding
// memory must never remove the guarantee. Sweeps both sources across the window
// where the old trigger flipped.
func TestSerialize_MonotonicInMemory(t *testing.T) {
	for _, src := range []string{"cgroup", "os-total"} {
		lost := false
		for mib := int64(1024); mib <= 65536; mib += 128 {
			lim := cgroupLimit(mib, false)
			if src == "os-total" {
				lim = hostLimit(mib)
			}
			b := ResolveBudget(0, lim)
			if !b.Serialize {
				lost = true
			} else if lost {
				t.Errorf("%s: gate came back ON at %d MiB after switching off at less memory — non-monotonic", src, mib)
				break
			}
		}
	}
}

// ── S3: the fraction depends on WHERE the limit was found, because that is what
// the fraction's premise claims.

func TestFraction_InheritedLimitIsTreatedAsShared(t *testing.T) {
	// A limit on a parent slice was drawn around a GROUP we belong to, not
	// around knomit. Claiming 0.8 of it claims most of someone else's budget —
	// and the walk takes the minimum over ancestors, so this is reachable
	// whenever an operator caps a slice knomit merely sits inside.
	own := ResolveBudget(0, cgroupLimit(8192, false))
	inherited := ResolveBudget(0, cgroupLimit(8192, true))
	if inherited.Tokens >= own.Tokens {
		t.Errorf("inherited %d >= own %d — a limit shared with other processes must yield a smaller claim",
			inherited.Tokens, own.Tokens)
	}
}

// TestFraction_ProtectiveLimitDoesNotIncreaseTheClaim is the perverse case in
// concrete form: an operator responds to the OOM by capping the slice knomit
// shares with other services. That must not make knomit claim MORE than it did
// with no limit at all.
func TestFraction_ProtectiveLimitDoesNotIncreaseTheClaim(t *testing.T) {
	before := ResolveBudget(0, hostLimit(15900))
	after := ResolveBudget(0, memlimit.Limit{
		Bytes: 8 << 30, Source: memlimit.SourceCgroupV2, Inherited: true, HostTotal: 15900 << 20,
	})
	if after.Tokens > before.Tokens {
		t.Errorf("adding a protective 8 GiB slice limit raised the budget from %d to %d tokens",
			before.Tokens, after.Tokens)
	}
}

func TestWorstCaseBatchBytes_MatchesTheLadder(t *testing.T) {
	if got := WorstCaseBatchBytes(16384); got != 1820<<20 {
		t.Errorf("WorstCaseBatchBytes(16384) = %d, want the measured 1820 MiB", got)
	}
	if WorstCaseBatchBytes(2048) >= WorstCaseBatchBytes(16384) {
		t.Error("worst-case bytes must increase with the budget")
	}
}

// ── Coherence pins on the constants themselves.

// TestResolve_IncidentMachineLandsOnTheDefault sanity-checks the constants
// against the machine that motivated all of this: ~15.9 GiB, no cgroup limit,
// knomit sharing the box. Deriving exactly the shipped default is evidence the
// fraction, the resident-model figure and the reserve are mutually coherent.
func TestResolve_IncidentMachineLandsOnTheDefault(t *testing.T) {
	got := ResolveBudget(0, hostLimit(15900))
	if got.Tokens != DefaultMaxBatchTokens || got.Clamped != "ceiling" {
		t.Errorf("got %d tokens (clamped %q), want the default at the ceiling", got.Tokens, got.Clamped)
	}

	// BOTH SIDES. The assertion above only fails when the constants drift in the
	// SAFE direction: on its own it reduces to
	// 0.25*T - resident - reserve >= 1820 MiB, which still holds if
	// sharedFraction were doubled to 0.50 — knomit claiming twice as much of a
	// shared host, the precise mistake behind the incident. Pinning a smaller
	// machine BELOW the ceiling fixes the boundary from underneath, so a
	// too-generous fraction moves this one and fails.
	smaller := ResolveBudget(0, hostLimit(12288))
	if smaller.Tokens >= DefaultMaxBatchTokens {
		t.Errorf("a 12 GiB shared host derived %d tokens — at or above the ceiling means the fraction "+
			"or the subtractive constants have grown too generous", smaller.Tokens)
	}
}

func TestResolve_ExplicitWins(t *testing.T) {
	got := ResolveBudget(4096, hostLimit(64000))
	if got.Tokens != 4096 || got.Source != "explicit" {
		t.Errorf("got %d tokens from %q, want 4096 from \"explicit\"", got.Tokens, got.Source)
	}
}

func TestResolve_NeverSizesUp(t *testing.T) {
	for _, lim := range []memlimit.Limit{cgroupLimit(64000, false), hostLimit(512000)} {
		if got := ResolveBudget(0, lim); got.Tokens > DefaultMaxBatchTokens {
			t.Errorf("%v -> %d, want <= %d", lim.Source, got.Tokens, DefaultMaxBatchTokens)
		}
	}
}
