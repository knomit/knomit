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
		if !got.Serialized() {
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
	if !got.Serialized() {
		t.Error("Serialize=false for an explicit budget under a cgroup — serialization derives from the ceiling, not from where Tokens came from")
	}
}

func TestSerialize_RoomyHostKeepsConcurrency(t *testing.T) {
	// The deployed machine. Its share (0.25 x 15.9 GiB, less the model and
	// knomit's own footprint) comfortably funds a batch, so it is not
	// floor-class and keeps concurrency — no behaviour change on a box we
	// confirmed healthy under real load.
	if got := ResolveBudget(0, hostLimit(15900)); got.Serialized() {
		t.Error("Serialize=true on the deployed 15.9 GiB host")
	}
	if got := ResolveBudget(0, hostLimit(8192)); got.Serialized() {
		t.Error("Serialize=true on an 8 GiB host — above the floor-class boundary")
	}
}

// TestSerialize_FloorClassHostSerializes: on a ~4 GiB shared laptop our share
// does not fund even a minimum batch. Before decision 12 this host was warned
// that "memory is the binding constraint" and simultaneously permitted
// unbounded overlap, because the WARN used our share and the gate used raw
// physical RAM. Same constants, opposite conclusions.
func TestSerialize_FloorClassHostSerializes(t *testing.T) {
	got := ResolveBudget(0, hostLimit(4096))
	if !got.Serialized() {
		t.Error("Serialize=false on a 4 GiB host that clamps to the floor")
	}
	if got.Clamped != "floor" {
		t.Errorf("Clamped = %q — the gate and the clamp must agree about this machine", got.Clamped)
	}
}

// TestSerialize_FloorClassIsProvenanceIndependent is why the rule is computed
// from the MACHINE rather than from the resolved budget. Keying on Clamped
// would re-couple the gate to provenance and reintroduce the defect where an
// operator who pins a budget loses the memory guarantee.
func TestSerialize_FloorClassIsProvenanceIndependent(t *testing.T) {
	got := ResolveBudget(8192, hostLimit(4096))
	if got.Tokens != 8192 {
		t.Errorf("Tokens = %d, want the configured 8192", got.Tokens)
	}
	if !got.Serialized() {
		t.Error("Serialize=false for an explicit budget on a floor-class host — the gate must key on the machine, not on where Tokens came from")
	}
}

// TestSerialize_FloorClassBoundary pins the threshold itself, so a constant
// drifting moves this test rather than silently moving the boundary.
func TestSerialize_FloorClassBoundary(t *testing.T) {
	if !ResolveBudget(0, hostLimit(7168)).Serialized() {
		t.Error("7 GiB should be floor-class")
	}
	if ResolveBudget(0, hostLimit(8192)).Serialized() {
		t.Error("8 GiB should not be floor-class")
	}
}

func TestSerialize_UnknownCeilingDoesNotSerialize(t *testing.T) {
	// We measured nothing, so we know nothing. An unmeasured host is not a
	// constrained one, and imposing the throughput cost on it is unfounded.
	got := ResolveBudget(0, memlimit.Limit{Source: memlimit.SourceNone})
	if got.Serialized() {
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
			if !b.Serialized() {
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

// TestSerialize_BeyondTheLadderGuaranteesRatherThanEstimates: WorstCaseBatchBytes
// saturates above the measured range, so comparing against it there would
// understate the real cost. Concretely: an explicit 32768 on an 8 GiB host
// modelled 5339 MiB for two runs when the real figure is ~9800 MiB — an ungated
// OOM. Reachable only via an operator value, since derivation never exceeds the
// default.
func TestSerialize_BeyondTheLadderGuaranteesRatherThanEstimates(t *testing.T) {
	for _, tokens := range []int{maxLadderTokens + 1, 65536, 262144} {
		got := ResolveBudget(tokens, hostLimit(64000))
		if !got.Serialized() {
			t.Errorf("explicit %d tokens on a 64 GiB host: Serialize=false — memory beyond the "+
				"measured ladder cannot be modelled, so it must be guaranteed instead", tokens)
		}
	}
}

// TestSerialize_UnreadableCeilingSerializes: SourceUnreadable means we know a
// limit may apply and could not read it — the opposite of SourceNone. Running
// the full default budget with unbounded overlap there would be behaviourally
// identical to the over-report the detector now refuses to make.
func TestSerialize_UnreadableCeilingSerializes(t *testing.T) {
	got := ResolveBudget(0, memlimit.Limit{Source: memlimit.SourceUnreadable})
	if !got.Serialized() {
		t.Error("Serialize=false on an unreadable ceiling — this is the one case where we know we are blind")
	}
	if got.Source != string(memlimit.SourceUnreadable) {
		t.Errorf("Source = %q, want %q so an operator can see it in the boot log", got.Source, memlimit.SourceUnreadable)
	}
}

// TestWorstCaseBatchBytes_IsAConsistentInverse checks the two ladder directions
// against each other across the whole range, not at a couple of points. Two
// separate pieces of safety math now depend on them agreeing.
func TestWorstCaseBatchBytes_IsAConsistentInverse(t *testing.T) {
	for availMiB := int64(0); availMiB <= 2200; availMiB++ {
		avail := availMiB * mib
		tokens := BudgetForBatchMemory(avail)
		if need := WorstCaseBatchBytes(tokens); need > avail && tokens > MinBatchTokens {
			t.Fatalf("avail=%d MiB -> %d tokens -> needs %d MiB: the inversion is not conservative",
				availMiB, tokens, need>>20)
		}
	}
}

// ── Decision 13: the gate is a CAPACITY, chosen per machine class.
// Tested as capacity x source, not each alone: the previous round's H4 was a
// two-axis property with only one axis pinned.

func TestBatchConcurrency_ByMachineClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		lim  memlimit.Limit
		want int
	}{
		{"cgroup is a hard wall", cgroupLimit(8192, false), 1},
		{"cgroup, inherited", cgroupLimit(8192, true), 1},
		{"floor-class host", hostLimit(4096), 1},
		{"roomy host is capped, not unbounded", hostLimit(15900), roomyHostConcurrency},
		{"very large host still capped", hostLimit(256000), roomyHostConcurrency},
		{"unreadable ceiling", memlimit.Limit{Source: memlimit.SourceUnreadable}, 1},
		{"unknown ceiling is unbounded", memlimit.Limit{Source: memlimit.SourceNone}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveBudget(0, tc.lim).BatchConcurrency; got != tc.want {
				t.Errorf("BatchConcurrency = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBatchConcurrency_RoomyHostBoundsMultiplicity is H1. The deployed machine
// derives the ceiling budget and previously took NO bound at all, so a 3-way
// overlap was 3 x 1820 = 5460 MiB of batch against the 3.97 GiB the fraction
// claims is knomit's entire share — and because the arena retains its
// high-water mark, that overshoot would have become resting RSS for the process
// lifetime rather than a spike.
func TestBatchConcurrency_RoomyHostBoundsMultiplicity(t *testing.T) {
	got := ResolveBudget(0, hostLimit(15900))
	if got.BatchConcurrency == 0 {
		t.Fatal("BatchConcurrency = 0 on the deployed host — unbounded overlap on a shared box")
	}
	// But NOT serialized: 2-way overlap is what that machine demonstrably
	// absorbed (4025 MiB measured across three repos), so capping at 2 codifies
	// observed behaviour rather than changing it. Only a third batch queues.
	if got.Serialized() {
		t.Error("Serialized on the deployed host — capping multiplicity must not become serializing")
	}
}

// TestBatchConcurrency_ExplicitAboveDefaultSerializes is H3, collapsed to ONE
// threshold. Previously three thresholds governed this axis — warn above 65536,
// ladder ends at 32768, forced gate above 32768 — leaving 16385..32768 modelled
// but neither warned nor bounded, and 40000 gated but unwarned.
func TestBatchConcurrency_ExplicitAboveDefaultSerializes(t *testing.T) {
	host := hostLimit(15900) // roomy: would otherwise get capacity 2
	if got := ResolveBudget(DefaultMaxBatchTokens, host); got.Serialized() {
		t.Errorf("explicit %d (exactly the default) serialized — the boundary is ABOVE the default",
			DefaultMaxBatchTokens)
	}
	for _, tokens := range []int{DefaultMaxBatchTokens + 1, 17000, 40000, 65536} {
		if got := ResolveBudget(tokens, host); !got.Serialized() {
			t.Errorf("explicit %d tokens: BatchConcurrency = %d, want 1 — above the shipped default the "+
				"operator has overridden our sizing and is least likely to have modelled overlap",
				tokens, got.BatchConcurrency)
		}
	}
}
