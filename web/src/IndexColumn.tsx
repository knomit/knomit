import type { CSSProperties, ReactNode } from 'react';
import { noMouseFocus } from './utils';

export interface IndexItem {
  id: string;
  title: string;
  /** Small marker after the title (a count, a licence pill). */
  tail?: ReactNode;
  /** The destructive group: the item is tinted #c77. */
  danger?: boolean;
  /** Something in this block needs attention: a red dot on the item. */
  problem?: boolean;
}

// IndexColumn is the left column of a page that is a list of blocks: the
// target's kind as its heading ("Repository", "Lens"), then one item per block.
// It is an index rather than a nav — every block is already on the page, so an
// item scrolls rather than swaps. The active item uses the selected rail row's
// treatment, so "this is where you are" reads the same in the rail and here.
export function IndexColumn({ heading, items, active, onPick }: {
  heading: string;
  items: IndexItem[];
  active: string;
  onPick: (id: string) => void;
}) {
  return (
    <nav aria-label={heading} style={nav}>
      <div data-testid="settings-index-heading" style={headingStyle}>{heading}</div>
      {items.map(it => {
        const on = active === it.id;
        return (
          <button
            key={it.id}
            type="button"
            data-testid={`toc-${it.id}`}
            onMouseDown={noMouseFocus}
            aria-current={on ? 'true' : undefined}
            style={item(on, !!it.danger)}
            onClick={() => onPick(it.id)}
          >
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{it.title}</span>
            {it.tail && <span style={{ marginLeft: 'auto', display: 'inline-flex' }}>{it.tail}</span>}
            {it.problem && (
              <span data-testid={`toc-${it.id}-problem`} aria-label="needs attention" style={{
                marginLeft: it.tail ? 6 : 'auto', width: 6, height: 6, borderRadius: '50%',
                background: '#f44336', flexShrink: 0,
              }} />
            )}
          </button>
        );
      })}
    </nav>
  );
}

// Sticky: the column scrolls only if the index outgrows the window, and the
// heading stays put while it does.
const nav: CSSProperties = { display: 'flex', flexDirection: 'column', gap: 1 };
const headingStyle: CSSProperties = {
  fontSize: 10.5, fontWeight: 600, textTransform: 'uppercase', letterSpacing: '.08em',
  color: '#777', padding: '0 10px 8px',
};
const item = (on: boolean, danger: boolean): CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 7, width: '100%', boxSizing: 'border-box',
  padding: '6px 10px', borderRadius: 4, fontSize: 12.5, textAlign: 'left',
  background: on ? '#1a231d' : 'transparent',
  color: on ? '#dfe9e2' : danger ? '#c77' : '#999',
  border: 'none', cursor: 'pointer',
  boxShadow: on ? 'inset 2px 0 0 #7c9' : 'none',
});
