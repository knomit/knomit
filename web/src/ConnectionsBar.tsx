export type EdgeDir = 'in' | 'out';

interface Props {
  incoming: number;
  outgoing: number;
  /** null = closed. */
  open: EdgeDir | null;
  onToggle: (dir: EdgeDir) => void;
  /** id of the drawer this bar controls, for aria-controls. */
  drawerId: string;
}

export const CONNECTIONS_BAR_WIDTH = 36;

const ACCENT: Record<EdgeDir, string> = { in: '#8af', out: '#fa8' };

/**
 * A 36px gutter holding the open fact's edge counts. Clicking a count opens the
 * drawer; the bar is the drawer's handle.
 *
 * It replaces a 300px column that spent its width rendering "none" twice for
 * the many facts that have no edges. The bar is FIXED WIDTH and always present
 * — including at 0/0 — so the prose never reflows as you move down the list,
 * which the previous "hide it when empty" answer could not give.
 *
 * It is also meant as an extension point: anything else that is ABOUT a fact
 * but not IN it (provenance, review state, backlink graphs) can take a slot
 * here and a drawer, without taking width from the prose again. Hence the
 * generic name.
 */
export function ConnectionsBar({ incoming, outgoing, open, onToggle, drawerId }: Props) {
  return (
    <div
      data-testid="connections-bar"
      style={{
        width: CONNECTIONS_BAR_WIDTH,
        boxSizing: 'border-box',
        flexShrink: 0,
        borderLeft: '1px solid #1f1f26',
        background: '#0a0a0a',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        padding: '10px 0',
        gap: 14,
      }}
    >
      <Item dir="in" count={incoming} open={open === 'in'} onToggle={onToggle} drawerId={drawerId} />
      <div style={{ width: 14, height: 1, background: '#222' }} />
      <Item dir="out" count={outgoing} open={open === 'out'} onToggle={onToggle} drawerId={drawerId} />
    </div>
  );
}

function Item({ dir, count, open, onToggle, drawerId }: {
  dir: EdgeDir;
  count: number;
  open: boolean;
  onToggle: (dir: EdgeDir) => void;
  drawerId: string;
}) {
  const label = dir === 'in' ? 'IN' : 'OUT';
  const noun = dir === 'in' ? 'incoming' : 'outgoing';
  const interactive = count > 0;

  // THE ACCENT LIVES ON THE ITEM, not on the count. The count, the label and
  // the selected indicator all resolve `currentColor` from here — put the
  // accent on a child instead and the indicator inherits the ambient #eee,
  // rendering a white bar next to a blue count.
  //
  // Zero is not a button: no accent, no hover, no pointer, no drawer. That is
  // how the bar says "nothing here" for the many facts that have nothing, and
  // it distinguishes an empty result from one that has not loaded.
  const style: React.CSSProperties = {
    color: interactive ? ACCENT[dir] : '#333',
    display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 3,
    padding: '4px 2px',
    background: open ? '#151515' : 'none',
    boxShadow: open ? 'inset 2px 0 0 currentColor' : 'none',
    border: 'none', outline: 'none',
    cursor: interactive ? 'pointer' : 'default',
    width: '100%',
    // index.css styles bare `button` with border-radius:8px and font-size:1em.
    // Unset here or the selected item renders as a rounded pill floating in the
    // gutter instead of a flush strip, and a 16px line box pads it taller than
    // the inert div next to it. A <button> and a <div> must be pixel-identical
    // in this bar: which one renders is a function of the COUNT, and the
    // difference between 0 and 1 must not move the layout.
    borderRadius: 0,
    fontSize: 11,
    lineHeight: 1,
  };

  const inner = (
    <>
      <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11, fontWeight: 600, color: 'currentColor' }}>
        {count}
      </span>
      {/*
        HORIZONTAL text. A vertical label (writing-mode: vertical-rl) at the
        size this column allows measured 2.03:1 against the background — the
        control whose entire premise is "IN or OUT?" could not legibly say
        which. 36px fits "OUT" horizontally at 9px.
      */}
      <span style={{
        fontFamily: 'var(--k-font-mono)', fontSize: 9, letterSpacing: '0.06em',
        textTransform: 'uppercase', color: 'currentColor', opacity: 0.8, lineHeight: 1,
      }}>{label}</span>
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
      aria-controls={drawerId}
      aria-label={`${count} ${noun} references`}
      style={style}
    >{inner}</button>
  );
}
