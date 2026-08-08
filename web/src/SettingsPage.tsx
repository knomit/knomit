import { useEffect, useRef, useState } from 'react';
import { noMouseFocus } from './utils';

// SettingsPage lays an entity's settings out as ONE scrolling column of headed
// blocks with a contents rail beside it.
//
// It replaces a stack of collapsed `Disclosure` cards. Those were the right
// shape inside a 900×620 dialog, where the pane's whole vertical budget was
// about two cards deep and anything you only read once had to fold away. In a
// mode that owns the window the calculus inverts: folding hides state (a
// failing remote behind a header you did not open) to save room there is now
// plenty of, and every fold costs a click on the way to the thing you came for.
//
// The contents rail is what replaces a tab strip. It is an index rather than a
// nav — every block is already on the page, so clicking a rail entry scrolls
// rather than swaps, and nothing is ever hidden behind a selection. It is also
// the growth signal: once it passes about a dozen entries the page has outgrown
// one column, and that is the moment to split blocks into nested rail sections
// rather than to keep adding.

export interface Section {
  /** Anchor id; also the scroll target and the rail entry's key. */
  id: string;
  title: string;
  /** Muted trailing clause on the heading — where the value lives, what writes it. */
  hint?: string;
  /** Right-aligned control(s) on the heading row: Edit, Rebuild, a status pill. */
  action?: React.ReactNode;
  /** Small marker for the rail entry (a licence pill, a health dot, a count). */
  tail?: React.ReactNode;
  /** Destructive group — tinted apart in both the block and the rail. */
  danger?: boolean;
  body: React.ReactNode;
}

export function SettingsPage({ sections, focus, testid }: {
  sections: Section[];
  /** Block to bring into view when the page opens. Set when arriving from an
   *  Overview cell, so "push rejected" lands you on Remote with the error in
   *  view rather than at the top of a page you then have to scan. */
  focus?: string;
  testid?: string;
}) {
  const [active, setActive] = useState<string>(sections[0]?.id ?? '');
  const bodyRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!focus) return;
    const node = bodyRef.current?.querySelector(`#${CSS.escape(focus)}`);
    if (!node) return;
    setActive(focus);
    node.scrollIntoView({ block: 'start' });
  }, [focus]);

  // Scroll-spy. Observed against the VIEWPORT (root: null) even though the
  // scrolling element is the detail column above us — scrolling an inner
  // container still changes viewport intersection, so this needs no reference
  // to whoever owns the overflow. The bottom margin biases the "current" block
  // toward the top of the pane, which is where the reader's eye is.
  useEffect(() => {
    const el = bodyRef.current;
    if (!el || typeof IntersectionObserver !== 'function') return;
    const seen = new Map<string, boolean>();
    const io = new IntersectionObserver(
      entries => {
        for (const e of entries) seen.set(e.target.id, e.isIntersecting);
        // First still-visible section in document order wins, so scrolling up
        // and down lands on the same answer rather than whichever fired last.
        const first = sections.find(s => seen.get(s.id));
        if (first) setActive(first.id);
      },
      { rootMargin: '0px 0px -70% 0px', threshold: 0 },
    );
    for (const s of sections) {
      const node = el.querySelector(`#${CSS.escape(s.id)}`);
      if (node) io.observe(node);
    }
    return () => io.disconnect();
  }, [sections]);

  const jump = (id: string) => {
    setActive(id);
    bodyRef.current?.querySelector(`#${CSS.escape(id)}`)?.scrollIntoView({ block: 'start', behavior: 'smooth' });
  };

  return (
    <div style={grid} data-testid={testid}>
      <div ref={bodyRef}>
        {sections.map(s => (
          <section key={s.id} id={s.id} style={block} data-testid={`block-${s.id}`}>
            <div style={blockHead}>
              <h4 style={s.danger ? blockTitleDanger : blockTitle}>{s.title}</h4>
              {s.hint && <span style={blockHint}>{s.hint}</span>}
              {s.action && <span style={blockAction}>{s.action}</span>}
            </div>
            {s.body}
          </section>
        ))}
      </div>

      {/* Sticky because the column it sits in is taller than the pane; it is
          the one thing that should not scroll away, since it is how you get
          back to where you were after an edit pushes the page down. */}
      <nav style={toc} aria-label="On this page">
        <div style={tocLabel}>On this page</div>
        {sections.map(s => (
          <button
            key={s.id}
            type="button"
            data-testid={`toc-${s.id}`}
            onMouseDown={noMouseFocus}
            aria-current={active === s.id ? 'true' : undefined}
            style={tocItem(active === s.id, !!s.danger)}
            onClick={() => jump(s.id)}
          >
            <span>{s.title}</span>
            {s.tail && <span style={{ marginLeft: 'auto' }}>{s.tail}</span>}
          </button>
        ))}
      </nav>
    </div>
  );
}

// ── styles ──
const grid: React.CSSProperties = {
  display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) 178px', gap: 26, alignItems: 'start',
};
// Blocks are separated by a rule rather than boxed: a page of seven bordered
// cards reads as seven unrelated things, and these are one object's settings.
const block: React.CSSProperties = {
  borderTop: '1px solid #202020', paddingTop: 16, marginTop: 16,
  display: 'flex', flexDirection: 'column', gap: 10,
};
const blockHead: React.CSSProperties = {
  display: 'flex', alignItems: 'baseline', gap: 10, flexWrap: 'wrap',
};
const blockTitle: React.CSSProperties = { margin: 0, fontSize: 13.5, fontWeight: 650, color: '#e6e6e6' };
const blockTitleDanger: React.CSSProperties = { ...blockTitle, color: '#c08a8a' };
const blockHint: React.CSSProperties = { fontSize: 11.5, color: '#6e6e6e' };
const blockAction: React.CSSProperties = { marginLeft: 'auto', display: 'flex', gap: 7, alignItems: 'center' };

const toc: React.CSSProperties = {
  position: 'sticky', top: 0, display: 'flex', flexDirection: 'column', gap: 2, marginTop: 16,
};
const tocLabel: React.CSSProperties = {
  fontSize: 9.5, letterSpacing: '0.13em', textTransform: 'uppercase', color: '#4e4e4e', padding: '0 8px 6px',
};
const tocItem = (on: boolean, danger: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 7, width: '100%',
  padding: '4px 9px', borderRadius: 4, fontSize: 11.5, textAlign: 'left',
  background: on ? '#1a231d' : 'transparent',
  color: on ? '#dfe9e2' : danger ? '#97696e' : '#808080',
  border: 'none', cursor: 'pointer',
  boxShadow: on ? 'inset 2px 0 0 -0.5px #7c9' : 'none',
});
