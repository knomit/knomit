import type { Components } from 'react-markdown';
import { markdownComponents, markdownPlugins } from './markdown';
import { FactLink, FactLinksMore, RepoLink } from './ChatFactChip';

// A table cell in a chat answer that is a bare number gets a 40x4px bar scaled
// to the largest number in its column. A remark plugin finds the numbers and
// the column maxima (it sees the whole table) and tags each cell with its
// share; the `td` component draws the bar.

interface Node { type: string; value?: string; children?: Node[]; data?: { hName?: string; hProperties?: Record<string, string | string[]> } }

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

// The streaming caret goes INSIDE the text it follows: at the end of the last
// paragraph (or heading, or list item), so it sits on the same line as the last
// word. Only a block with no text to end (a table, a code block) gets a line of
// its own.
const CARET: Node = {
  type: 'caret',
  data: { hName: 'span', hProperties: { className: ['k-chat-caret'], 'data-testid': 'chat-caret', 'aria-hidden': 'true' } },
};
const INLINE_END = new Set(['paragraph', 'heading']);
const WRAPS = new Set(['root', 'list', 'listItem', 'blockquote']);
function placeCaret(n: Node): boolean {
  if (INLINE_END.has(n.type)) { (n.children ??= []).push({ ...CARET }); return true; }
  const last = WRAPS.has(n.type) ? n.children?.[n.children.length - 1] : undefined;
  return !!last && placeCaret(last);
}
function remarkCaret() {
  return (root: Node) => {
    if (!placeCaret(root)) (root.children ??= []).push({ type: 'paragraph', children: [{ ...CARET }] });
  };
}

/** The plugins and components a chat answer renders with. */
export const chatPlugins = [...markdownPlugins, remarkTableBars];
/** The same, for the block a turn is still streaming into: the caret follows its last word. */
export const chatLivePlugins = [...chatPlugins, remarkCaret];

export const chatComponents: Components = {
  ...markdownComponents,
  // Inline code remarkFactLinks marked: a resolved fact is a fact link, a repo
  // name a repo chip. Every other code span is code.
  code({ node: _node, children, ...rest }) {
    void _node;
    const { ['data-fact']: fact, ['data-repo']: repo, ...props } = rest as Record<string, unknown>;
    if (typeof fact === 'string') return <FactLink handle={fact} />;
    if (typeof repo === 'string') return <RepoLink name={repo} />;
    return <code {...props}>{children}</code>;
  },
  span({ node: _node, children, ...rest }) {
    void _node;
    const { ['data-fact-more']: more, ...props } = rest as Record<string, unknown>;
    if (more !== undefined) return <FactLinksMore>{children}</FactLinksMore>;
    return <span {...props}>{children}</span>;
  },
  // A table wider than the column (a long cell, and the column narrows beside
  // the side panel) scrolls inside its own box instead of being cut off at the
  // panel's edge.
  table({ node: _node, children, ...rest }) {
    void _node;
    return <div className="k-chat-table-scroll" data-testid="chat-table-scroll"><table {...rest}>{children}</table></div>;
  },
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
