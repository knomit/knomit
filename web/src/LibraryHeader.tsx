import type { LibrarySort } from './state';
import type { ComponentType } from 'react';
import { TreeIcon, StopwatchIcon, TargetIcon, ChevronLeftIcon } from './icons';
import { OverflowCrumb } from './OverflowCrumb';

interface Props {
  count: number;
  /** Ancestor chain, root-first, display-ready. Empty at the root. */
  ancestors: string[];
  /** Current folder name. null at the root → the header renders the context instead. */
  leaf: string | null;
  /**
   * Whether the Library column is too narrow to keep the root ancestor.
   *
   * A BOOLEAN, not the pixel width the design called for. `leftWidth` changes on
   * every frame of a splitter drag, and Library sits under a memoized LeftPanel
   * — threading pixels would re-render the whole list ~40 times per drag, which
   * is precisely what App.resilience.test.tsx's "dragging the splitter does not
   * re-render the library" exists to catch. The layout only ever asks whether we
   * are past the threshold, so only the answer crosses the boundary.
   */
  narrow: boolean;
  sort: LibrarySort;
  searchActive: boolean;
  onSortChange: (sort: LibrarySort) => void;
  /** Leave the search and fall back to the mode the reader was in before it. */
  onExitSearch?: () => void;
  canBack: boolean;
  onBack: () => void;
  /** Index into the FULL ancestors array (not the collapsed layout). */
  onJumpAncestor: (index: number) => void;
}

type IconType = ComponentType<{ color: string; size?: number }>;

// Sort axes render as theme-colored glyphs (tree = path hierarchy, stopwatch =
// recency, target = best-match relevance); the label survives as the tooltip
// and accessible name.
const segments: { value: LibrarySort; label: string; testid: string; Icon: IconType }[] = [
  { value: 'path',      label: 'Path',      testid: 'sort-path',      Icon: TreeIcon },
  { value: 'recent',    label: 'Recent',    testid: 'sort-recent',    Icon: StopwatchIcon },
  { value: 'relevance', label: 'Relevance', testid: 'sort-relevance', Icon: TargetIcon },
];

type AncItem =
  | { kind: 'seg'; index: number }
  | { kind: 'overflow'; indices: number[] };

// A location line is tighter than a trail, so MAX_INLINE is 3 where
// TrailBreadcrumb's is 4: root › … › immediate parent.
const MAX_INLINE = 3;

/**
 * Which ancestors render inline and which collapse behind the "…".
 *
 * DROPS THE ROOT BEFORE THE PARENT when narrow. Going up one level is the
 * common move, so the immediate parent is the segment worth the pixels; the
 * root is one click away inside the overflow menu, which is why the "…" has to
 * be a menu and not a text ellipsis.
 *
 * Indices are always into the FULL array — that is the contract onJumpAncestor
 * depends on, and the off-by-one this shape exists to prevent.
 */
function layoutAncestors(n: number, narrow: boolean): AncItem[] {
  if (n === 0) return [];
  const keepRoot = !narrow;
  if (n <= (keepRoot ? MAX_INLINE : 2)) {
    return Array.from({ length: n }, (_, index) => ({ kind: 'seg', index }));
  }
  const last = n - 1;
  const hidden = Array.from({ length: keepRoot ? last - 1 : last }, (_, k) => (keepRoot ? k + 1 : k));
  return keepRoot
    ? [{ kind: 'seg', index: 0 }, { kind: 'overflow', indices: hidden }, { kind: 'seg', index: last }]
    : [{ kind: 'overflow', indices: hidden }, { kind: 'seg', index: last }];
}

/**
 * The Library's header IS its location: where you are, one level of ancestry,
 * and the way back.
 *
 * It used to spend its row on the word "Library" plus a `scoped` boolean that
 * never named its value, while the actual location lived in a `path:` chip in
 * the FilterBar — mounted in the right column, on the far side of the splitter
 * from the list it scopes. `path` is walk semantics (ancestors, a position, a
 * direction) wearing set semantics' clothing.
 *
 * The root slot has no directory name to show, so it names what the list IS —
 * see ROOT_LABEL. It is a fallback, never a title: a named folder always wins.
 *
 * Two lines in the row the header already occupied, so vertical cost is zero.
 * Both lines render in every state — at the root the ancestor slot carries the
 * context and the leaf slot names the list — because a header that grows a
 * line on the first navigation shifts the whole list under the cursor.
 */
// What the root is called on each tab. The root is the one level with no
// directory name of its own, so the slot names what the list is instead — and
// the list is a genuinely different thing per sort. A single fixed label had to
// be vague enough to cover all three, and "All facts" was: it sat over eight
// DIRECTORIES and zero facts under Path sort.
//
// No "All" on the paged tabs. The count beside it is the server's total for the
// CURRENT query, so with a domain chip set "All facts · 137" would contradict
// itself; "Facts · 137" is true either way.
const ROOT_LABEL: Record<LibrarySort, string> = {
  path: 'Ontology',
  recent: 'Facts',
  relevance: 'Matches',
};

export function LibraryHeader({
  count, ancestors, leaf, narrow, sort, searchActive,
  onSortChange, onExitSearch, canBack, onBack, onJumpAncestor,
}: Props) {
  const visible = segments.filter(s => s.value !== 'relevance' || searchActive);
  const items = layoutAncestors(ancestors.length, narrow);

  return (
    <div
      data-testid="library-header"
      style={{
        display: 'flex', alignItems: 'center', gap: 6,
        padding: '5px 10px 6px 6px', borderBottom: '1px solid #1a1a1a', background: '#0f0f0f',
        fontSize: 11, color: '#888', fontFamily: 'var(--k-font-mono)',
      }}
    >
      <button
        data-testid="library-back"
        onClick={() => { if (canBack) onBack(); }}
        disabled={!canBack}
        aria-label="Back"
        title="Back (⌘[ or Backspace)"
        style={{
          background: 'none', border: 'none', outline: 'none',
          width: 20, height: 26, padding: 0, flexShrink: 0,
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          cursor: canBack ? 'pointer' : 'default',
        }}
      >
        <ChevronLeftIcon color={canBack ? '#888' : '#2e2e2e'} size={13} />
      </button>

      {/* Location: ancestors above, current folder below. */}
      <div style={{ flex: 1, minWidth: 0, fontFamily: 'var(--k-font-mono)' }}>
        <div
          data-testid="library-ancestors"
          style={{ fontSize: 10.5, color: '#888', display: 'flex', gap: 2, marginBottom: 1, alignItems: 'center' }}
        >
          {leaf === null ? (
            // At the root there is no ancestry to show, and the repo/branch is
            // already named in the TopBar — repeating it here spent the line on
            // a duplicate. The slot still renders (a non-breaking space, so the
            // box is identical to the populated one) because both lines must
            // exist in every state or the header changes height on the first
            // navigation and the list shifts under the cursor.
            <span data-testid="library-context" aria-hidden="true" style={ancestorPlaceholder}>{'\u00a0'}</span>
          ) : (
            items.map((item, pos) => (
              <span key={item.kind === 'seg' ? `s${item.index}` : 'overflow'} style={ancestorItem}>
                {pos > 0 && <span style={{ color: '#555', flexShrink: 0 }}>/</span>}
                {item.kind === 'seg' ? (
                  <button
                    data-testid="ancestor-seg"
                    onClick={() => onJumpAncestor(item.index)}
                    title={ancestors[item.index]}
                    style={ancestorBtn}
                    onMouseEnter={e => { e.currentTarget.style.background = '#222'; }}
                    onMouseLeave={e => { e.currentTarget.style.background = 'none'; }}
                  >{ancestors[item.index]}</button>
                ) : (
                  <OverflowCrumb
                    indices={item.indices}
                    label={i => ancestors[i]}
                    onJump={onJumpAncestor}
                  />
                )}
              </span>
            ))
          )}
        </div>
        <div
          data-testid="library-leaf"
          style={{
            fontSize: 12, color: '#7c9', letterSpacing: 0.4,
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
          }}
        >{leaf ?? ROOT_LABEL[sort]}</div>
      </div>

      <span data-testid="library-count" style={{ fontSize: 10, color: '#666', fontFamily: 'var(--k-font-mono)', flexShrink: 0 }}>{count}</span>

      {/* Sort axes as borderless glyphs — state reads through color alone:
          accent green = active, muted = idle, brighter on hover. No pill, no
          selection border. The icon inherits the button's CSS color. */}
      <div style={{ display: 'flex', gap: 4, flexShrink: 0 }}>
        {visible.map(seg => {
          const active = sort === seg.value;
          // While searching, order is forced to relevance — Path/Recent can't
          // change it and clicking one only resets the open fact. Disable them
          // so Relevance is the only live control.
          const disabled = searchActive && seg.value !== 'relevance';
          // ...and give that one live control a job. Relevance is DERIVED from
          // searchActive, so "set sort to relevance" was a no-op that still
          // nulled the open fact — the reader got the dashboard beside a list
          // of matches. Pressed while lit, it now leaves the search and drops
          // back to whichever mode was showing before.
          const exits = searchActive && seg.value === 'relevance' && !!onExitSearch;
          const color = disabled ? '#3a3a3a' : active ? '#7c9' : '#666';
          return (
            <button
              key={seg.value}
              data-testid={seg.testid}
              disabled={disabled}
              onClick={() => {
                if (disabled) return;
                if (exits) onExitSearch(); else onSortChange(seg.value);
              }}
              aria-label={exits ? 'Clear search' : `Sort by ${seg.label}`}
              aria-pressed={active}
              title={disabled ? 'Sorting is disabled while searching'
                : exits ? 'Clear search and go back'
                : `Sort by ${seg.label}`}
              onMouseEnter={e => { if (!disabled && !active) e.currentTarget.style.color = '#aaa'; }}
              onMouseLeave={e => { if (!disabled && !active) e.currentTarget.style.color = color; }}
              style={{
                background: 'none',
                color,
                border: 'none',
                outline: 'none',
                padding: '3px 5px', borderRadius: 4,
                cursor: disabled ? 'not-allowed' : 'pointer',
                display: 'inline-flex', alignItems: 'center',
              }}
            ><seg.Icon color="currentColor" size={14} /></button>
          );
        })}
      </div>
    </div>
  );
}

// Only the LAST item may shrink. Without this, a flex row of overflow:hidden
// children all resolve min-width to 0 and shrink PROPORTIONALLY — every segment
// and every separator truncates at once, giving `co… / … / distributed-sys…`
// instead of one ellipsized name. The count rule above handles depth; this
// handles a single absurdly long segment.
const ancestorItem: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 2, flex: '0 0 auto', minWidth: 0,
};
// The empty root slot must occupy EXACTLY the box a populated one does, or the
// header changes height on the first navigation. Matching the segment button's
// font size and padding rather than hard-coding a pixel height keeps them equal
// under any font metrics — a magic number was off by 2.25px here.
const ancestorPlaceholder: React.CSSProperties = {
  padding: '2px 3px', fontSize: 10.5, lineHeight: 'normal',
};
// #888 at 10.5px on #0f0f0f, not #666: #666 measures 3.34:1, below the 4.5:1
// floor, and these are the click targets the whole header rests on.
// TrailBreadcrumb's non-last CrumbButton is the precedent. Padding gives a hit
// area larger than the glyphs.
const ancestorBtn: React.CSSProperties = {
  cursor: 'pointer', background: 'none', border: 'none', color: '#888',
  padding: '2px 3px', borderRadius: 3, fontSize: 10.5,
  fontFamily: 'var(--k-font-mono)',
  whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
  maxWidth: 120,
};
