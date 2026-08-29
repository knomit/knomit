import { MOTIF_GLYPH } from './utils';
import type { ResolvedMotif } from './useMotifClusters';

/**
 * One motif in the fact header's edges row — and the whole cell is the button.
 *
 * WHY THE NAME IS NEVER CLIPPED. Everywhere else in this app, text that will not
 * fit gets an ellipsis; the path on the row above does exactly that. A motif is
 * not a label, it is a compressed claim whose meaning sits in its last word, so
 * cutting it does not make it vaguer — it makes it say something else, often the
 * opposite, in a place where a reader has no reason to doubt it.
 * `failure-presents-as-…` reads as the ordinary case; `name-implies-…` reads as
 * the reassuring one. Both are the inverse of the motif they came from. So a
 * name that does not fit is DROPPED by the row (into its `+N` cell), never
 * truncated here, and this component sets no overflow behaviour at all.
 *
 * THE FOUR STATES ARE ConnectionsCell's, deliberately not new ones. That cell
 * has taught this app's readers what a header counter does since it shipped:
 * coloured means live, grey means inert, the cursor turns, the title names the
 * action, and an open cell wears an inset underline on its bottom edge pointing
 * at the panel it opened. Inventing a cue here would mean two vocabularies for
 * one gesture.
 *
 * THE ACCENT IS SET ON THE CELL, so the glyph, the name, the count and the open
 * marker all resolve `currentColor` from one place. Put it on a child and the
 * marker inherits the ambient colour and renders grey beside a coloured name —
 * the failure kb/decisions/ui/connections/header-cells calls out by name.
 *
 * The colour itself is no colour: near-white, where every neighbour in the row
 * wears a hue that names a subject (cited-by entity blue, cites episode orange,
 * version domain green). A motif is the one thing there making no claim about
 * what a fact is ABOUT, and its colourlessness is that statement.
 */
export function MotifCell({ motif, open, onToggle, panelId }: {
  /** null when the fact carries no motifs — the inert zero cell. */
  motif: ResolvedMotif | null;
  open: boolean;
  onToggle: (motif: string) => void;
  panelId: string;
}) {
  // Zero renders INERT, not absent: a fact with no motifs and a fact with three
  // must present the same row, or the header changes shape as you move down a
  // list. The same reason ConnectionsCell draws its own zero.
  if (!motif) {
    return (
      <div data-testid="motif-cell" data-interactive="false" data-state="zero" style={cell('#333')}>
        <span aria-hidden style={glyph}>{MOTIF_GLYPH}</span>
        <span style={label}>same motif</span>
        <b style={count}>0</b>
      </div>
    );
  }

  const failed = motif.status === 'error';
  const loading = motif.status === 'loading';
  const color = failed ? '#f66' : open ? '#ffffff' : '#c8cfdb';

  // The count has three answers and they must stay apart. A failed fetch shows a
  // warning and a pending one shows ellipsis dots; neither may become `0`, which
  // would assert that nothing else in the corpus carries this shape.
  const readout = failed ? '!' : loading ? '···' : String(motif.cluster?.carrier_count ?? '');

  const title = failed
    ? `Count unavailable: ${motif.error ?? 'request failed'}`
    : loading
      ? `${motif.motif} — loading`
      : `${motif.cluster?.carrier_count ?? 0} facts share this motif`;

  return (
    <button
      type="button"
      data-testid="motif-cell"
      data-interactive="true"
      data-state={motif.status}
      data-motif={motif.motif}
      onClick={() => onToggle(motif.motif)}
      aria-expanded={open}
      aria-controls={panelId}
      // The error on hover: a reader should not have to open the panel to learn
      // that this is a failure rather than a fact about the corpus.
      title={title}
      style={{ ...cell(color), ...(open ? openMarker : null), cursor: 'pointer' }}
    >
      <span aria-hidden style={glyph}>{MOTIF_GLYPH}</span>
      {/* No overflow, no ellipsis, no max-width — see the note above. */}
      <span data-testid="motif-name" style={label}>{motif.motif}</span>
      <b style={{ ...count, ...(failed ? { color: '#f66' } : null) }}>{readout}</b>
    </button>
  );
}

// index.css styles bare `button` with border-radius:8px and font-size:1em; both
// are unset here for the same reason ConnectionsCell unsets them — a rounded
// 16px pill beside an 11px chip, and a height that differs from the <div> the
// zero state renders as, which would move the header as a fact gained a motif.
const cell = (color: string): React.CSSProperties => ({
  color,
  display: 'inline-flex',
  alignItems: 'center',
  gap: 5,
  background: 'none',
  border: 'none',
  outline: 'none',
  borderRadius: 0,
  padding: '3px 9px',
  whiteSpace: 'nowrap',
  fontFamily: 'var(--k-font-mono)',
  fontSize: 11,
  lineHeight: 1.4,
  cursor: 'default',
});

const openMarker: React.CSSProperties = {
  background: '#151515',
  // The panel hangs BELOW, so the open marker sits on the bottom edge.
  boxShadow: 'inset 0 -2px 0 currentColor',
};

const glyph: React.CSSProperties = { fontSize: 10, opacity: 0.9, color: '#6d7788' };
const label: React.CSSProperties = { color: 'inherit' };
const count: React.CSSProperties = { fontWeight: 600, color: '#7f8b9c' };
