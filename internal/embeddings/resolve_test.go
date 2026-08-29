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

// TestSerialize_EveryDetectedCeilingIsCapped replaces an earlier test that
// asserted roomy hosts keep concurrency. That exemption was withdrawn in
// decision 14: measured at the shape a roomy host actually runs (8x2048
// batches), capacity 2 vs 1 was 65.42s vs 65.25s — ratio 1.00, no benefit —
// while spending 109% of the share sharedFraction claims, retained for the
// process lifetime.
func TestSerialize_EveryDetectedCeilingIsCapped(t *testing.T) {
	for _, lim := range []memlimit.Limit{
		hostLimit(4096), hostLimit(15900), hostLimit(256000),
		cgroupLimit(2048, false), cgroupLimit(65536, true),
		{Source: memlimit.SourceUnreadable},
	} {
		if got := ResolveBudget(0, lim); !got.Serialized() {
			t.Errorf("%s (%d MiB): BatchConcurrency = %d, want 1",
				lim.Source, lim.Bytes>>20, got.BatchConcurrency)
		}
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

// TestFloorClassBoundary pins the threshold, which now drives the operator
// warning rather than the cap.
func TestFloorClassBoundary(t *testing.T) {
	if !FloorClass(hostLimit(7168)) {
		t.Error("7 GiB should be floor-class")
	}
	if FloorClass(hostLimit(8192)) {
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

// TestBatchConcurrency_MonotonicInMemory is the property the original clamping
// rule violated: more memory must never LOOSEN the bound. Asserted on the
// capacity, not on a boolean — a boolean cannot distinguish capacity 2 from
// capacity 0, which is precisely the distinction decision 13 introduced, so a
// host jumping from 2 to unbounded would have passed the old form.
func TestBatchConcurrency_MonotonicInMemory(t *testing.T) {
	// Looseness order: 1 (tightest) < 2 < 0 (unbounded).
	looseness := func(n int) int {
		if n == 0 {
			return 1 << 30
		}
		return n
	}
	for _, src := range []string{"cgroup", "os-total"} {
		prev := 0
		for mib := int64(1024); mib <= 65536; mib += 128 {
			lim := cgroupLimit(mib, false)
			if src == "os-total" {
				lim = hostLimit(mib)
			}
			got := looseness(ResolveBudget(0, lim).BatchConcurrency)
			if got < prev {
				t.Fatalf("%s at %d MiB: bound TIGHTENED as memory grew (%d after %d)", src, mib, got, prev)
			}
			prev = got
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
		{"roomy host is capped like any detected host", hostLimit(15900), 1},
		{"very large host still capped", hostLimit(256000), 1},
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

// TestBatchConcurrency_RoomyHostBoundsMultiplicity is H1: the deployed machine
// derives the ceiling budget and previously took NO bound at all, so a 3-way
// overlap was 5460 MiB of batch against the 3975 MiB the fraction claims is
// knomit's entire share — and because the arena retains its high-water mark,
// that overshoot becomes resting RSS for the process lifetime rather than a
// spike.
//
// It is capped at 1, not 2. Capacity 2 was measured at the shape the cap
// actually governs (8x2048 batches, one build) as 65.42s concurrent vs 65.25s
// serialized — ratio 1.00, no benefit — while spending 109% of the claimed
// share against 63% at capacity 1. The cap and the sizing fraction agree at
// exactly one value.
func TestBatchConcurrency_RoomyHostBoundsMultiplicity(t *testing.T) {
	got := ResolveBudget(0, hostLimit(15900))
	if got.BatchConcurrency != 1 {
		t.Errorf("BatchConcurrency = %d on the deployed host, want 1", got.BatchConcurrency)
	}
}

// TestFloorClass_DrivesTheWarningNotTheCap: floor-class no longer changes the
// cap (every detected ceiling is 1), but it still identifies a host small
// enough to change how knomit runs, which an operator is told about.
func TestFloorClass_DrivesTheWarningNotTheCap(t *testing.T) {
	small, large := hostLimit(4096), hostLimit(15900)
	if !FloorClass(small) {
		t.Error("4 GiB host is not floor-class")
	}
	if FloorClass(large) {
		t.Error("15.9 GiB host is floor-class")
	}
	if a, b := ResolveBudget(0, small).BatchConcurrency, ResolveBudget(0, large).BatchConcurrency; a != b {
		t.Errorf("cap differs by floor-class (%d vs %d) — it should not any more", a, b)
	}
	// A cgroup source IS floor-class when its own share cannot fund a batch.
	// An earlier version of this assertion said the opposite — that FloorClass
	// "is a physical-RAM notion" — which is exactly the guard that silently
	// denied every container its floor warning. See TestFloorClass_EverySource.
	if !FloorClass(cgroupLimit(2048, false)) {
		t.Error("a 2 GiB dedicated cgroup is floor-class: 0.8 x 2048 does not fund the model plus a batch")
	}
}

// TestBatchConcurrency_ExplicitAboveDefaultSerializes is H3, collapsed to ONE
// threshold. Previously three thresholds governed this axis — warn above 65536,
// ladder ends at 32768, forced gate above 32768 — leaving 16385..32768 modelled
// but neither warned nor bounded, and 40000 gated but unwarned.
func TestBatchConcurrency_ExplicitAboveDefaultSerializes(t *testing.T) {
	// Isolated on an UNKNOWN ceiling, the only class that is otherwise
	// unbounded — on any detected host the cap would be 1 regardless, so this
	// rule cannot be observed there at all.
	//
	// Worth knowing: that makes this the ONLY fixture under which the
	// explicit>default branch is reachable. A rule guarding an operator override
	// now hangs on a single test class, so if the unknown-ceiling case ever
	// changes, this rule silently loses its coverage rather than failing.
	unknown := memlimit.Limit{Source: memlimit.SourceNone}
	if got := ResolveBudget(DefaultMaxBatchTokens, unknown); got.BatchConcurrency != 0 {
		t.Errorf("explicit %d (exactly the default) on an unmeasured host: cap = %d, want 0 — the boundary is ABOVE the default",
			DefaultMaxBatchTokens, got.BatchConcurrency)
	}
	for _, tokens := range []int{DefaultMaxBatchTokens + 1, 17000, 40000, 65536} {
		if got := ResolveBudget(tokens, unknown); !got.Serialized() {
			t.Errorf("explicit %d tokens: cap = %d, want 1 — above the shipped default the operator has "+
				"overridden our sizing and is least likely to have modelled overlap",
				tokens, got.BatchConcurrency)
		}
	}
}

// TestFloorClass_EverySource is how the os-total-only guard got through: every
// FloorClass assertion used a physical-RAM fixture, so a predicate that
// returned false for all containers looked correct. A 2 GiB container is the
// single deployment shape this whole feature exists for, and it was the one
// losing its warning.
func TestFloorClass_EverySource(t *testing.T) {
	for _, tc := range []struct {
		name string
		lim  memlimit.Limit
		want bool
	}{
		{"2 GiB dedicated cgroup", cgroupLimit(2048, false), true},
		{"3 GiB dedicated cgroup has room", cgroupLimit(3072, false), false},
		{"4 GiB inherited slice", cgroupLimit(4096, true), true},
		{"8 GiB dedicated cgroup", cgroupLimit(8192, false), false},
		{"4 GiB bare host", hostLimit(4096), true},
		{"15.9 GiB bare host", hostLimit(15900), false},
		{"unknown ceiling is not floor-class", memlimit.Limit{Source: memlimit.SourceNone}, false},
		{"unreadable ceiling is not floor-class", memlimit.Limit{Source: memlimit.SourceUnreadable}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FloorClass(tc.lim); got != tc.want {
				t.Errorf("FloorClass = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFloorClass_MatchesDerivedClamp pins the property the predicate exists to
// provide: it is what Clamped WOULD have been had the budget been derived. That
// is what makes it usable for an explicit budget, where Clamped is always
// "none".
func TestFloorClass_MatchesDerivedClamp(t *testing.T) {
	for _, lim := range []memlimit.Limit{
		cgroupLimit(2048, false), cgroupLimit(3072, false), cgroupLimit(8192, true),
		hostLimit(4096), hostLimit(8192), hostLimit(15900),
	} {
		derived := ResolveBudget(0, lim).Clamped == "floor"
		if got := FloorClass(lim); got != derived {
			t.Errorf("%s %d MiB: FloorClass=%v but a derived budget clamps %q",
				lim.Source, lim.Bytes>>20, got, ResolveBudget(0, lim).Clamped)
		}
	}
}
