import type { EdgeDir } from './utils';
import { EDGE_ACCENT, EDGE_GLYPH } from './utils';

interface Props {
  dir: EdgeDir;
  count: number;
  open: boolean;
  onToggle: (dir: EdgeDir) => void;
  /** id of the panel this cell controls, for aria-controls. */
  panelId: string;
}

/**
 * One edge-count cell in the fact header's control menu, which reads
 * `↙2 ↗3 ⏱v1 ⊗` — connections, then version, then retract.
 *
 * Connections used to be a 300px column, then a 36px gutter with a side drawer.
 * Both spent permanent horizontal space on something a reader consults
 * occasionally; putting the counts beside the version chip costs nothing that
 * was not already header, and the panel they open is transient.
 *
 * ZERO IS NOT A BUTTON: no accent, no hover, no pointer, no panel. It renders
 * rather than disappearing so the header's control row does not reflow between
 * a fact with edges and one without.
 */
export function ConnectionsCell({ dir, count, open, onToggle, panelId }: Props) {
  const interactive = count > 0;
  const noun = dir === 'in' ? 'incoming' : 'outgoing';

  // The accent lives HERE, so the glyph, the count and the open indicator all
  // resolve currentColor from one place. On a child instead, the indicator
  // inherits the ambient colour and renders grey beside a coloured count.
  const style: React.CSSProperties = {
    color: interactive ? EDGE_ACCENT[dir] : '#333',
    display: 'inline-flex', alignItems: 'center', gap: 3,
    padding: '1px 5px',
    background: open ? '#151515' : 'none',
    // The panel hangs BELOW, so the open marker sits on the bottom edge — the
    // horizontal analogue of the side rail's leading strip.
    boxShadow: open ? 'inset 0 -2px 0 currentColor' : 'none',
    border: 'none', outline: 'none',
    cursor: interactive ? 'pointer' : 'default',
    // index.css styles bare `button` with border-radius:8px and font-size:1em.
    // Unset both, or the cell renders as a rounded pill at 16px next to an 11px
    // chip — and the <button> and the <div> a zero renders as would be
    // different heights, so a count going 0 → 1 would move the header.
    borderRadius: 3,
    fontFamily: 'var(--k-font-mono)',
    fontSize: 11,
    lineHeight: 1.4,
  };

  const inner = (
    <>
      <span style={{ fontSize: 10, opacity: 0.9 }}>{EDGE_GLYPH[dir]}</span>
      <span style={{ fontWeight: 600 }}>{count}</span>
    </>
  );

  if (!interactive) {
    return (
      <div data-testid={`connections-${dir}`} data-interactive="false" style={style}>{inner}</div>
    );
  }

  return (
    <button
      type="button"
      data-testid={`connections-${dir}`}
      data-interactive="true"
      onClick={() => onToggle(dir)}
      aria-expanded={open}
      aria-controls={panelId}
      aria-label={`${count} ${noun} references`}
      title={`${count} ${noun} references`}
      style={style}
    >{inner}</button>
  );
}
