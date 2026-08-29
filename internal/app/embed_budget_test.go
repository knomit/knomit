package app

import (
	"testing"

	"knomit/internal/embeddings"
	"knomit/internal/memlimit"
)

const gib = int64(1) << 30

// TestResolveMaxBatchTokens_ExplicitWins: an operator who set a value gets it
// verbatim, with no detection at all. Auto-sizing may only ever fill in for
// silence — overriding a configured number would make the knob a suggestion.
func TestResolveMaxBatchTokens_ExplicitWins(t *testing.T) {
	got := resolveMaxBatchTokens(4096, memlimit.Limit{Bytes: 2 * gib, Source: memlimit.SourceCgroupV2})
	if got.Tokens != 4096 {
		t.Errorf("Tokens = %d, want the configured 4096", got.Tokens)
	}
	if got.Source != "explicit" {
		t.Errorf("Source = %q, want \"explicit\"", got.Source)
	}
}

// TestResolveMaxBatchTokens_SmallContainerClampsDown is the case the whole
// feature exists for. A 2 GiB container cannot hold the shipped default's
// worst batch on top of the resident model, and sizing from /proc/meminfo
// there would read the HOST's RAM and reproduce the original OOM.
func TestResolveMaxBatchTokens_SmallContainerClampsDown(t *testing.T) {
	got := resolveMaxBatchTokens(0, memlimit.Limit{Bytes: 2 * gib, Source: memlimit.SourceCgroupV2})
	if got.Tokens >= embeddings.DefaultMaxBatchTokens {
		t.Errorf("Tokens = %d, want well below the default %d in a 2 GiB container",
			got.Tokens, embeddings.DefaultMaxBatchTokens)
	}
	if got.Clamped != "floor" {
		t.Errorf("Clamped = %q, want \"floor\" — 2 GiB leaves no room for a full batch", got.Clamped)
	}
}

// TestResolveMaxBatchTokens_IncidentMachineLandsOnTheDefault sanity-checks the
// constants against the machine that motivated all of this: ~15.9 GiB, no
// cgroup limit, knomit sharing the box. The derived value landing exactly on
// the shipped default is evidence the fraction and reserve are coherent — if
// this drifts, one of the three constants moved without the others.
func TestResolveMaxBatchTokens_IncidentMachineLandsOnTheDefault(t *testing.T) {
	got := resolveMaxBatchTokens(0, memlimit.Limit{Bytes: 15900 * (1 << 20), Source: memlimit.SourceOSTotal})
	if got.Tokens != embeddings.DefaultMaxBatchTokens {
		t.Errorf("Tokens = %d, want %d on the incident machine", got.Tokens, embeddings.DefaultMaxBatchTokens)
	}
	if got.Clamped != "ceiling" {
		t.Errorf("Clamped = %q, want \"ceiling\"", got.Clamped)
	}
}

// TestResolveMaxBatchTokens_FractionDependsOnSource: the SOURCE implies the
// sharing model. A cgroup limit means an operator sized this container for
// knomit, so most of it is ours; falling through to physical RAM means an
// unknown host with unknown co-tenants (the incident had two), so we claim far
// less of it. One fraction for both would be wrong in both directions.
func TestResolveMaxBatchTokens_FractionDependsOnSource(t *testing.T) {
	const same = 8 * gib
	cg := resolveMaxBatchTokens(0, memlimit.Limit{Bytes: same, Source: memlimit.SourceCgroupV2})
	os := resolveMaxBatchTokens(0, memlimit.Limit{Bytes: same, Source: memlimit.SourceOSTotal})
	if cg.Tokens <= os.Tokens {
		t.Errorf("cgroup %d vs os-total %d — a dedicated container must yield a larger budget than a shared host of the same size",
			cg.Tokens, os.Tokens)
	}
}

// TestResolveMaxBatchTokens_UnknownLimitUsesFixedDefault: detection failing is
// not an error. Embeddings are mandatory, so an undetectable memory ceiling
// must degrade to the shipped default rather than block startup.
func TestResolveMaxBatchTokens_UnknownLimitUsesFixedDefault(t *testing.T) {
	got := resolveMaxBatchTokens(0, memlimit.Limit{Source: memlimit.SourceNone})
	if got.Tokens != embeddings.DefaultMaxBatchTokens {
		t.Errorf("Tokens = %d, want the fixed default %d", got.Tokens, embeddings.DefaultMaxBatchTokens)
	}
	if got.Source != string(memlimit.SourceNone) {
		t.Errorf("Source = %q, want %q", got.Source, memlimit.SourceNone)
	}
}

// TestResolveMaxBatchTokens_NeverExceedsDefault is the clamp-DOWN-only rule at
// the layer an operator sees: however large the machine, auto-sizing may only
// reduce the shipped default.
func TestResolveMaxBatchTokens_NeverExceedsDefault(t *testing.T) {
	for _, lim := range []memlimit.Limit{
		{Bytes: 64 * gib, Source: memlimit.SourceCgroupV2},
		{Bytes: 512 * gib, Source: memlimit.SourceOSTotal},
	} {
		if got := resolveMaxBatchTokens(0, lim); got.Tokens > embeddings.DefaultMaxBatchTokens {
			t.Errorf("%v -> %d, want <= %d", lim.Source, got.Tokens, embeddings.DefaultMaxBatchTokens)
		}
	}
}

// The serialization gate. Batch inference is serialized IFF the budget was
// DERIVED and came in BELOW the ceiling — i.e. the machine actually
// constrained us. Both polarities are pinned, because getting either wrong is
// silent: a false negative loses the memory guarantee on the hosts that need
// it, and a false positive imposes a measured ~37% throughput cost (4 workers,
// 4x2048) on healthy hosts that do not.

func TestSerializeGate_OffWhenTheMachineDidNotConstrainUs(t *testing.T) {
	// Derived, but landed on the ceiling — the host is roomy. Concurrency is
	// affordable here and measurably faster, so leave it alone. This is the
	// incident machine's own case: 15.9 GiB derives exactly the default, and it
	// survived 4.2 GiB of real overlap.
	got := resolveMaxBatchTokens(0, memlimit.Limit{Bytes: 15900 * (1 << 20), Source: memlimit.SourceOSTotal})
	if got.Serialize {
		t.Errorf("Serialize = true at the ceiling (%d) — a roomy host must keep concurrency", got.Tokens)
	}
}

func TestSerializeGate_OnWhenClampedBelowTheCeiling(t *testing.T) {
	// A 2 GiB container: overlapping peaks are exactly what would OOM it, and
	// the memory concurrency would spend is not there to spend.
	got := resolveMaxBatchTokens(0, memlimit.Limit{Bytes: 2 * gib, Source: memlimit.SourceCgroupV2})
	if !got.Serialize {
		t.Errorf("Serialize = false at %d tokens (clamped %q) — a constrained host needs the per-process bound",
			got.Tokens, got.Clamped)
	}
}

func TestSerializeGate_OffForAnExplicitValue(t *testing.T) {
	// An explicit value opts out of DETECTION, so it opts out of the gate too:
	// the operator owns their number and we have not measured their machine.
	// Note the value here is below the ceiling — proving the gate keys on
	// provenance, not merely on magnitude.
	got := resolveMaxBatchTokens(4096, memlimit.Limit{Bytes: 2 * gib, Source: memlimit.SourceCgroupV2})
	if got.Serialize {
		t.Error("Serialize = true for an explicitly configured budget — explicit opts out of detection AND of the gate")
	}
}

func TestSerializeGate_OffWhenTheCeilingIsUnknown(t *testing.T) {
	// Undetectable memory means we know nothing about this host. Falling back
	// to the fixed default is not evidence of constraint, so do not impose the
	// throughput cost on a machine we failed to measure.
	got := resolveMaxBatchTokens(0, memlimit.Limit{Source: memlimit.SourceNone})
	if got.Serialize {
		t.Error("Serialize = true on an undetectable ceiling — an unmeasured host is not a constrained one")
	}
}
