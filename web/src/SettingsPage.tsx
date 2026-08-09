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
  // TWO writers, one winner. `pinned` is what you asked for by clicking a rail
  // entry (or arriving with `focus`); `spied` is what scrolling infers. The pin
  // wins until you scroll under your own steam, because an inference must never
  // overrule an instruction — and because the blocks near the end of a page can
  // never reach the top of the pane, so scrolling to them cannot agree with the
  // click that got you there.
  const [pinned, setPinned] = useState<string | null>(null);
  const [spied, setSpied] = useState<string>(sections[0]?.id ?? '');
  const active = pinned ?? spied;
  const bodyRef = useRef<HTMLDivElement>(null);

  // Section identity, not the array: the sections are rebuilt on every render of
  // the owning pane, so depending on the array itself would tear these listeners
  // down and rebuild them continuously.
  //
  // NUL as the separator because no id can contain one; written as the escape
  // rather than a literal control character, which would make git classify this
  // whole file as binary and stop diffing, blaming and merging it.
  const ids = sections.map(s => s.id).join('\u0000');

  // Sections can arrive LATE — RepoDetail only pushes `license` once its GET
  // resolves — so this depends on section identity as well as on `focus`.
  // Without that, arriving from the Licence cell would look for a block that
  // does not exist yet, give up, and never try again.
  //
  // `focused` makes it fire at most once per requested block. A plain re-run on
  // `ids` would yank the page back every time a later section landed, undoing a
  // scroll the user had already made for themselves.
  const focused = useRef<string | null>(null);
  useEffect(() => {
    if (!focus || focused.current === focus) return;
    const node = bodyRef.current?.querySelector(`#${CSS.escape(focus)}`);
    if (!node) return;
    focused.current = focus;
    setPinned(focus);
    node.scrollIntoView({ block: 'start' });
  }, [focus, ids]);

  // Scroll-spy, by POSITION rather than intersection. The previous version
  // observed intersection with the top 30% of the viewport, which had a blind
  // spot it could not report its way out of: a page bottoms out before its last
  // blocks reach that band, so Index and Danger zone could never become current
  // however far you scrolled.
  //
  // Reading positions against the scroll container fixes that directly — the
  // current block is the last one whose top has passed the container's top, and
  // once the container is scrolled to the end the LAST block is current by
  // definition, wherever it happens to sit.
  useEffect(() => {
    const el = bodyRef.current;
    const container = scrollParentOf(el);
    if (!el || !container) return;

    const compute = () => {
      const list = ids.split('\u0000').filter(Boolean);
      if (list.length === 0) return;
      // Scrolled to the end: the last block is what you are looking at, even
      // though it never reaches the top.
      if (container.scrollHeight - container.scrollTop - container.clientHeight <= 2) {
        setSpied(list[list.length - 1]);
        return;
      }
      const top = container.getBoundingClientRect().top;
      let current = list[0];
      for (const id of list) {
        const node = el.querySelector(`#${CSS.escape(id)}`);
        if (!node) continue;
        // A small allowance so a block sitting just below the fold's edge does
        // not flicker between itself and the one above on a one-pixel scroll.
        if (node.getBoundingClientRect().top - top <= 24) current = id;
        else break;
      }
      setSpied(current);
    };

    // Deferred to the next frame: reading layout synchronously in an effect
    // measures the frame that has not been painted yet.
    const first = requestAnimationFrame(compute);
    container.addEventListener('scroll', compute, { passive: true });
    window.addEventListener('resize', compute);
    // The page can GROW without the window resizing and without anything
    // scrolling — an editor opens, a long note arrives — which moves every
    // block below it. `ids` alone does not catch that: a lens page's sections
    // are the same five whatever their contents do.
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(compute);
    ro?.observe(el);
    return () => {
      cancelAnimationFrame(first);
      container.removeEventListener('scroll', compute);
      window.removeEventListener('resize', compute);
      ro?.disconnect();
    };
  }, [ids]);

  // Releasing the pin listens for the INPUTS that scroll — wheel, touch, the
  // keys — not for `scroll` itself, which the pin's own smooth animation fires
  // and would use to cancel itself a frame after being set.
  useEffect(() => {
    if (pinned === null) return;
    const release = () => setPinned(null);
    window.addEventListener('wheel', release, { passive: true });
    window.addEventListener('touchmove', release, { passive: true });
    window.addEventListener('keydown', release);
    return () => {
      window.removeEventListener('wheel', release);
      window.removeEventListener('touchmove', release);
      window.removeEventListener('keydown', release);
    };
  }, [pinned]);

  const jump = (id: string) => {
    setPinned(id);
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

/**
 * scrollParentOf finds the element that actually scrolls this page.
 *
 * SettingsPage does not own its overflow — the detail column above it does —
 * and asking for it as a prop would make every caller thread a ref through for
 * one internal detail. Walking up to the nearest scrollable ancestor keeps that
 * knowledge here, where it is used.
 *
 * Overflow alone, NOT "is it overflowing right now": the nearest auto/scroll
 * ancestor is the scroll container by CSS's own definition, whether or not it
 * has anything to scroll yet. Requiring scrollHeight > clientHeight meant a
 * page that happened to fit at mount got no listeners at all, and the rail then
 * stayed frozen on the first block for the rest of that page's life.
 */
function scrollParentOf(el: HTMLElement | null): HTMLElement | null {
  for (let n = el?.parentElement ?? null; n; n = n.parentElement) {
    const overflow = getComputedStyle(n).overflowY;
    if (overflow === 'auto' || overflow === 'scroll') return n;
  }
  return null;
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
// `alignSelf: center` takes the action OUT of the heading's baseline row. The
// title and its hint still align on their shared baseline; the action does not
// get a vote. It used to, and that moved the title by 2px whenever a control
// swapped for one with different innards — the description block's pencil
// becoming Save and Cancel, where an icon's baseline is its box edge and a
// button's is the text inside it.
const blockAction: React.CSSProperties = {
  marginLeft: 'auto', alignSelf: 'center', display: 'flex', gap: 7, alignItems: 'center',
};

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
