import { useRef, useState } from 'react';
import type { RefObject } from 'react';
import { useDismiss } from './hooks';
import { MOTIF_GLYPH, typeStyles, defaultTypeStyle } from './utils';
import { subjectSummary } from './motifSubject';
import type { ResolvedMotif } from './useMotifClusters';
import type { MotifAlias, MotifCluster } from './api';

export const MOTIF_PANEL_WIDTH = 420;

/**
 * The panel a motif name opens, hanging from the cell that opened it.
 *
 * THE FIRST CLICK INSPECTS. Leaving for the twenty-six other facts is a second,
 * deliberate click on the button at the bottom — nothing irreversible happens
 * on a click you made to find out what something was. It is also what keeps the
 * edges row honest: every cell in that border inspects THIS fact, which is why
 * retract was left outside it, and a pivot lands on a corpus-wide query.
 *
 * The shell is ConnectionsPanel's, deliberately: same width, same surfaces, the
 * same `esc` chip. A reader who has opened one has opened both. There is no
 * version tab, because the version is not a panel-opener and does not sit with
 * the cells this hangs from.
 */
export function MotifPanel({ motifs, focused, onClose, onPivot, menuRef, onMouseEnter, onMouseLeave, id }: {
  /** Every motif on the fact, in the row's order — the panel and the row must
   *  agree about one list, so the caller passes the ordering it drew. */
  motifs: ResolvedMotif[];
  /** Which one is expanded: the name that was clicked, or null when the panel
   *  was opened from the `+N` cell and nothing in particular is focused. */
  focused: string | null;
  onClose: () => void;
  onPivot: (motif: string) => void;
  menuRef: RefObject<HTMLElement | null>;
  onMouseEnter: () => void;
  onMouseLeave: () => void;
  id: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useDismiss(true, onClose, [ref, menuRef]);

  // Nothing focused (the +N route) opens on the first, which is the
  // highest-carrier motif since the caller sorted them.
  const open = focused ?? motifs[0]?.motif ?? null;

  return (
    <div
      id={id}
      ref={ref}
      data-testid="motif-panel"
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
      style={{
        position: 'absolute', top: '100%', left: 0, marginTop: 6,
        width: MOTIF_PANEL_WIDTH, maxHeight: 420,
        background: '#101010', border: '1px solid #2f2f2f', borderRadius: 6,
        boxShadow: '0 10px 30px rgba(0,0,0,0.6)', zIndex: 20,
        overflow: 'hidden', display: 'flex', flexDirection: 'column', textAlign: 'left',
      }}
    >
      <div style={{
        padding: '8px 10px', background: '#0d0d0d', borderBottom: '1px solid #1a1a1a',
        fontFamily: 'var(--k-font-mono)', fontSize: 10, letterSpacing: '0.06em',
        textTransform: 'uppercase', display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0,
      }}>
        <span style={{ color: '#e8eef6' }}>{MOTIF_GLYPH} same motif</span>
        <span style={{ fontSize: 13, fontWeight: 600, color: '#e8eef6' }}>{motifs.length}</span>
        <span style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{
            fontSize: 9, color: '#555', border: '1px solid #2a2a2a',
            borderRadius: 3, padding: '0 4px', textTransform: 'none',
          }}>esc</span>
          <button type="button" data-testid="motif-panel-close" onClick={onClose}
            aria-label="Close motifs"
            style={{
              background: 'none', border: 'none', outline: 'none', padding: 0,
              borderRadius: 0, color: '#666', cursor: 'pointer', fontSize: 13, lineHeight: 1,
            }}>×</button>
        </span>
      </div>

      <div style={{ flex: 1, overflowY: 'auto', minHeight: 0 }}>
        {motifs.map(m => (
          <MotifSection key={m.motif} motif={m} expanded={m.motif === open} onPivot={onPivot} />
        ))}
      </div>
    </div>
  );
}

/** One motif in the panel: expanded, or a single collapsed line. */
function MotifSection({ motif, expanded, onPivot }: {
  motif: ResolvedMotif; expanded: boolean; onPivot: (motif: string) => void;
}) {
  const [showAliases, setShowAliases] = useState(false);
  const c = motif.cluster;

  if (!expanded) {
    return (
      <div data-testid="motif-section" data-motif={motif.motif} data-expanded="false"
        style={{ padding: '10px 12px', borderTop: '1px solid #1a1a1a', opacity: 0.6 }}>
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 9 }}>
          <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#c8cfdb' }}>{motif.motif}</span>
          <span style={{
            marginLeft: 'auto', fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#6d7788',
          }}>{c ? carrierLabel(c.carrier_count) : motif.status === 'error' ? '!' : '···'}</span>
        </div>
      </div>
    );
  }

  return (
    <div data-testid="motif-section" data-motif={motif.motif} data-expanded="true"
      style={{
        padding: '11px 12px 12px', borderTop: '1px solid #1a1a1a',
        // The expanded one is marked down its left edge rather than by a fill,
        // so the panel reads as one list with a focus rather than two lists.
        boxShadow: 'inset 3px 0 0 #e8eef6', background: '#141418',
        display: 'flex', flexDirection: 'column', gap: 8,
      }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 9 }}>
        <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#fff' }}>{motif.motif}</span>
        <span data-testid="motif-carrier-count" style={{
          marginLeft: 'auto', fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#7f8b9c',
        }}>
          {/* carrier_count, not df: this is the number of facts the button below
              will actually land on, so it has to be the query's own answer. */}
          {c ? carrierLabel(c.carrier_count) : motif.status === 'error' ? 'count unavailable' : '···'}
        </span>
      </div>

      {motif.status === 'error' && (
        <div data-testid="motif-error" style={{ fontSize: 12, lineHeight: 1.5, color: '#e0a0a0' }}>
          This motif could not be read.
        </div>
      )}

      {/* The blind definition — one sentence describing the mechanism, written
          without seeing the facts that carry it. Absent when the vocabulary
          pass has not written one: an absence, not a hole to fill with a
          placeholder saying there is nothing here. */}
      {c?.definition && (
        <div data-testid="motif-definition" style={{
          fontSize: 12.5, lineHeight: 1.58, color: '#a3acbb',
        }}>{c.definition}</div>
      )}
      {c?.definition && c.definition_state === 'stale' && (
        // Lowercase, no colour: a sentence written before another spelling
        // joined is a normal state of a living vocabulary, not a fault, and it
        // is still the best description anyone has of the cluster.
        <div data-testid="motif-definition-state" style={{
          fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#5f6a7c',
        }}>written before a spelling joined · interim</div>
      )}

      {c && c.carriers.length > 0 && <SubjectLine cluster={c} />}

      {c && c.carriers.length > 0 && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 5, marginTop: 2 }}>
          {c.carriers.slice(0, 3).map(car => {
            const ts = (car.type && typeStyles[car.type]) || defaultTypeStyle;
            return (
              <div key={car.path} data-testid="motif-carrier"
                style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                <span style={{ color: ts.color, fontSize: 10, flex: 'none' }}>{ts.icon}</span>
                <span style={{
                  fontSize: 11.5, color: '#b9c1cd',
                  overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                }}>{car.title}</span>
              </div>
            );
          })}
        </div>
      )}

      {c && (
        <div style={{
          display: 'flex', alignItems: 'center', gap: 12,
          marginTop: 4, paddingTop: 10, borderTop: '1px solid #1a1a1a',
        }}>
          {/* The only thing here that leaves the fact, and it says so.
              NOT OFFERED AT ONE CARRIER, which is the commonest case rather
              than an edge one — 36 of this base's 73 clusters are carried by a
              single fact. The list it would open is the fact already on screen,
              so the button would be a dead end that also reads "1 carriers".
              The count line above says "only this fact" instead: an answer,
              and a real thing to know about a name minted once. */}
          {c.carrier_count > 1 && (
            <button type="button" data-testid="motif-pivot" onClick={() => onPivot(motif.motif)}
              style={{
                fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#dbe2ec',
                background: 'none', border: '1px solid #3a4150', borderRadius: 3,
                padding: '4px 10px', cursor: 'pointer', outline: 'none',
              }}>Open all {c.carrier_count} carriers &nbsp;→</button>
          )}
          <button type="button" data-testid="motif-spellings"
            onClick={() => setShowAliases(v => !v)}
            disabled={c.aliases.length < 2}
            style={{
              marginLeft: 'auto', background: 'none', border: 'none', padding: 0,
              borderRadius: 0, outline: 'none',
              fontFamily: 'var(--k-font-mono)', fontSize: 10,
              color: c.aliases.length < 2 ? '#4d5665' : '#8a93a3',
              cursor: c.aliases.length < 2 ? 'default' : 'pointer',
            }}>
            {c.members.length === 1 ? '1 spelling' : `${c.members.length} spellings`}
          </button>
        </div>
      )}

      {showAliases && c && <Aliases aliases={c.aliases} canonical={c.canonical} />}
    </div>
  );
}

/** How many facts carry this motif, in words that parse at every value.
 *
 *  One is not "1 carriers", and it is not a number worth printing either: the
 *  useful thing at one is that there is nowhere to go, which is what the phrase
 *  says. Below the fold this is also what tells a reader the shape has been
 *  named once — an authoring-hygiene fact, not a fault. */
function carrierLabel(n: number): string {
  return n === 1 ? 'only this fact' : `${n} carriers`;
}

/** Which parts of the system this motif's other facts are about.
 *
 *  Marked approximate on purpose: the server caps `carriers` at twenty, so this
 *  is computed from a preview and can only understate. The pivot's own heading
 *  says the same thing from the full landed rows, where it is complete. */
function SubjectLine({ cluster }: { cluster: MotifCluster }) {
  const s = subjectSummary(cluster.carriers.map(c => c.path), 3);
  if (s.shown.length === 0) return null;
  const partial = cluster.carriers.length < cluster.carrier_count;
  return (
    <div data-testid="motif-subjects" style={{
      fontFamily: 'var(--k-font-mono)', fontSize: 10, lineHeight: 1.7, color: '#5f6a7c',
    }}>
      across {s.shown.join(' · ')}
      {s.more > 0 && <span style={{ color: '#4d5665' }}> · +{s.more} more</span>}
      {partial && <span style={{ color: '#4d5665' }}> · of the first {cluster.carriers.length}</span>}
    </div>
  );
}

/** Why two spellings are one motif — with the judge's written reason.
 *
 *  The provenance surface: "why are these the same thing" has an answer, and it
 *  is one click away rather than in the way. */
function Aliases({ aliases, canonical }: { aliases: MotifAlias[]; canonical: string }) {
  return (
    <div data-testid="motif-aliases" style={{
      display: 'flex', flexDirection: 'column', gap: 9,
      marginTop: 4, paddingTop: 9, borderTop: '1px solid #1a1a1a',
    }}>
      {aliases.map(a => (
        <div key={a.motif}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 9 }}>
            <span style={{
              fontFamily: 'var(--k-font-mono)', fontSize: 11,
              color: a.motif === canonical ? '#e8eef6' : '#c8cfdb',
            }}>{a.motif}</span>
            <span style={{
              marginLeft: 'auto', fontFamily: 'var(--k-font-mono)', fontSize: 9.5, color: '#5f6a7c',
            }}>{a.motif === canonical ? 'the corpus’s name' : a.method}</span>
          </div>
          {a.rationale && (
            <div data-testid="motif-rationale" style={{
              fontSize: 12, lineHeight: 1.58, color: '#8a93a3',
              borderLeft: '2px solid #2a2f38', paddingLeft: 10, marginTop: 6,
            }}>{a.rationale}</div>
          )}
        </div>
      ))}
    </div>
  );
}
