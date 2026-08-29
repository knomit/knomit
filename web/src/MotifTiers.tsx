import type { MotifCluster } from './api';
import type { MotifMatch } from './api';

/** The three rungs, loosest last. `exact` already matches at the CLUSTER level,
 *  so the two looser tiers are for deliberately exploring a vocabulary — they
 *  admit names that merely share word-stems, which are not carriers of this
 *  motif at all. */
const TIERS: { tier: MotifMatch; label: string }[] = [
  { tier: 'exact', label: 'this motif' },
  { tier: 'stem', label: 'stem' },
  { tier: 'token-2', label: 'token-2' },
];

/**
 * Whether the `stem` rung could add anything — answered without asking the
 * server.
 *
 * `exact` matches every spelling in the cluster. `stem` matches on the
 * mechanical stemmed-token key, which IS how the cluster was formed — so the
 * only way the two can differ is a cluster a judge merged out of two mechanical
 * groups. No alias with `method: "judge"` therefore means stem ≡ exact,
 * provably, for free, from a response the panel has already fetched.
 *
 * This matters more than it sounds: on a young vocabulary almost no cluster has
 * a judge merge, so almost every stem rung is dead — and a control that
 * visibly does nothing reads as broken rather than as an answer about the
 * corpus. Knowing before the click is what lets the rung say so.
 */
export function stemCanAdd(cluster: MotifCluster | undefined): boolean {
  return !!cluster?.aliases.some(a => a.method === 'judge');
}

/**
 * The widen control: three rungs, each carrying the delta it would actually add.
 *
 * A rung that would add nothing renders INERT — present, greyed, not a button —
 * which is the rule the zero connection counter already follows. Nothing here
 * is hard-coded: emptiness is a property of a young vocabulary, not of the
 * feature, and the moment two spellings are merged the middle rung comes alive
 * on its own. Baking in "stem is useless" would be a corpus property frozen as
 * a constant, which this project forbids.
 */
export function MotifTiers({ active, exactCount, stemDelta, tokenDelta, onPick }: {
  active: MotifMatch;
  exactCount: number | null;
  /** null while unknown; 0 means measured and empty. */
  stemDelta: number | null;
  tokenDelta: number | null;
  onPick: (tier: MotifMatch) => void;
}) {
  const deltaFor = (t: MotifMatch) =>
    t === 'exact' ? exactCount : t === 'stem' ? stemDelta : tokenDelta;

  return (
    <div data-testid="motif-tiers" style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
      <span style={{
        display: 'inline-flex', alignItems: 'stretch',
        border: '1px solid #2a2a2a', borderRadius: 3, overflow: 'hidden',
      }}>
        {TIERS.map(({ tier, label }, i) => {
          const d = deltaFor(tier);
          const on = tier === active;
          // Live when it is the current tier, or when it would actually add
          // rows. `null` (not yet measured) stays live: a rung is not declared
          // dead until something has looked.
          const live = on || d === null || d > 0;
          const readout = d === null ? '' : on ? String(d ?? '') : d > 0 ? `+${d}` : '—';
          return (
            <span key={tier} style={{ display: 'inline-flex' }}>
              {i > 0 && <span style={{ width: 1, background: '#2a2a2a' }} />}
              <button
                type="button"
                data-testid={`motif-tier-${tier}`}
                data-live={String(live)}
                data-active={String(on)}
                disabled={!live}
                onClick={() => live && onPick(tier)}
                title={live
                  ? (on ? 'The motif itself' : `Also match ${label}`)
                  : 'Nothing in this vocabulary matches more loosely'}
                style={{
                  display: 'inline-flex', alignItems: 'center', gap: 6,
                  fontFamily: 'var(--k-font-mono)', fontSize: 10,
                  padding: '3px 10px', border: 'none', outline: 'none', borderRadius: 0,
                  background: on ? '#20232a' : 'transparent',
                  color: on ? '#e8eef6' : live ? '#c8cfdb' : '#3f4753',
                  cursor: live && !on ? 'pointer' : 'default',
                }}
              >
                {label}
                {readout && <span style={{ color: live ? '#7f8b9c' : '#3f4753' }}>{readout}</span>}
              </button>
            </span>
          );
        })}
      </span>
      {active !== 'exact' && (
        // The widened state is marked three ways — here, on the chip, and on
        // every row the widening let in. A reader must never mistake a widened
        // list for the motif's own carriers.
        <span data-testid="motif-tiers-note" style={{ fontSize: 10, color: '#8a93a3' }}>
          includes near matches
        </span>
      )}
    </div>
  );
}

/**
 * Which spelling let this row in, or null if it is a real carrier.
 *
 * Computed from data already on the row: a fact carries its motif spellings, so
 * a row whose spellings include none of the cluster's members was admitted by
 * the loosened tier rather than by the motif. Naming the admitting spelling is
 * what stops a widened list reading as the cluster — the row is visibly here on
 * a technicality, and says which one.
 */
export function admittedBy(rowMotifs: string[] | undefined, members: string[]): string | null {
  if (!rowMotifs?.length) return null;
  const set = new Set(members.map(m => m.toLowerCase()));
  if (rowMotifs.some(m => set.has(m.toLowerCase()))) return null;
  return rowMotifs[0];
}
