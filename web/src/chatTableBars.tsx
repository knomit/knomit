import type { Components } from 'react-markdown';
import { markdownComponents, markdownPlugins } from './markdown';

// A table cell in a chat answer that is a bare number gets a 40x4px bar scaled
// to the largest number in its column. A remark plugin finds the numbers and
// the column maxima (it sees the whole table) and tags each cell with its
// share; the `td` component draws the bar.

interface Node { type: string; value?: string; children?: Node[]; data?: { hProperties?: Record<string, string> } }

const NUMBER = /^[~≈]?\s*(-?\d+(?:\.\d+)?)$/;

const textOf = (n: Node): string => (n.type === 'text' || n.type === 'inlineCode' ? n.value ?? '' : (n.children ?? []).map(textOf).join(''));

/** numberOf is the value of a bare-number cell, or null. */
export function numberOf(text: string): number | null {
  const m = NUMBER.exec(text.trim());
  return m ? Number(m[1]) : null;
}

function tagTable(table: Node): void {
  const rows = (table.children ?? []).filter(r => r.type === 'tableRow').slice(1);
  const cols = Math.max(0, ...rows.map(r => r.children?.length ?? 0));
  for (let c = 0; c < cols; c++) {
    const cells = rows.map(r => r.children?.[c]).filter((x): x is Node => !!x);
    const vals = cells.map(x => numberOf(textOf(x)));
    const max = Math.max(0, ...vals.map(v => (v === null ? 0 : Math.abs(v))));
    if (max === 0) continue;
    cells.forEach((cell, i) => {
      const v = vals[i];
      if (v === null) return;
      cell.data = { ...cell.data, hProperties: { ...cell.data?.hProperties, 'data-bar': String(Math.abs(v) / max) } };
    });
  }
}

function walk(n: Node): void {
  if (n.type === 'table') tagTable(n);
  n.children?.forEach(walk);
}

function remarkTableBars() {
  return walk;
}

/** The plugins and components a chat answer renders with. */
export const chatPlugins = [...markdownPlugins, remarkTableBars];

export const chatComponents: Components = {
  ...markdownComponents,
  td({ node: _node, children, ...rest }) {
    void _node;
    const share = (rest as Record<string, unknown>)['data-bar'];
    const { ['data-bar']: _bar, ...props } = rest as Record<string, unknown>;
    void _bar;
    if (share === undefined) return <td {...props}>{children}</td>;
    return (
      <td {...props}>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
          {children}
          <span data-testid="chat-cell-bar" data-share={String(share)} aria-hidden="true"
            style={{ display: 'inline-block', width: 40, height: 4, borderRadius: 2, background: '#262626', overflow: 'hidden' }}>
            <span style={{ display: 'block', height: '100%', width: `${Number(share) * 100}%`, background: '#6fbf92' }} />
          </span>
        </span>
      </td>
    );
  },
};
