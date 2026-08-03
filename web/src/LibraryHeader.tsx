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
  /** Shown in the ancestor slot at the root: "core · main", or the lens name. */
  contextLabel: string;
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
 * Two lines in the row the header already occupied, so vertical cost is zero.
 * Both lines render in every state — at the root the ancestor slot carries the
 * context and the leaf slot reads "All facts" — because a header that grows a
 * line on the first navigation shifts the whole list under the cursor.
 */
export function LibraryHeader({
  count, ancestors, leaf, contextLabel, narrow, sort, searchActive,
  onSortChange, canBack, onBack, onJumpAncestor,
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
            <span data-testid="library-context" style={ancestorLast}>{contextLabel}</span>
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
        >{leaf ?? 'All facts'}</div>
      </div>

      <span style={{ fontSize: 10, color: '#666', fontFamily: 'var(--k-font-mono)', flexShrink: 0 }}>{count}</span>

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
          const color = disabled ? '#3a3a3a' : active ? '#7c9' : '#666';
          return (
            <button
              key={seg.value}
              data-testid={seg.testid}
              disabled={disabled}
              onClick={() => { if (!disabled) onSortChange(seg.value); }}
              aria-label={`Sort by ${seg.label}`}
              aria-pressed={active}
              title={disabled ? 'Sorting is disabled while searching' : `Sort by ${seg.label}`}
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
const ancestorLast: React.CSSProperties = {
  flex: '0 1 auto', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
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
