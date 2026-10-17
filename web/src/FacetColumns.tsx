import { createContext, useLayoutEffect, useRef, useState } from 'react';
import type { CSSProperties, MouseEvent as ReactMouseEvent, ReactNode } from 'react';
import { useFacetLayout } from './facetLayout';

// The column split a facet body shares with Browse: a left column exactly as
// wide as the Library, the same splitter, and a right column that takes the
// rest. The width and the drag handler are App's (`leftPanelWidth`,
// `startSplitterDrag`), so dragging the splitter on Settings moves Browse's
// too and the edge sits at the same x whichever facet is showing. The width
// and handler arrive through facetLayout.ts's context.

/** FacetSplitter is the drag handle between the left and right columns: a 1px
 *  rule with a wider hit zone. Browse and every facet column use THIS one, so
 *  the rule is drawn at the same x on all of them. */
export function FacetSplitter({ onMouseDown, testId }: { onMouseDown?: (e: ReactMouseEvent) => void; testId: string }) {
  return (
    <div
      data-testid={testId}
      onMouseDown={onMouseDown}
      title={onMouseDown ? 'Drag to resize' : undefined}
      style={{
        width: 4, marginLeft: -2, marginRight: -2,
        cursor: onMouseDown ? 'ew-resize' : 'default', flexShrink: 0, zIndex: 1,
        background: 'transparent',
        borderLeft: '1px solid #222',
      }}
      onMouseEnter={e => { if (onMouseDown) (e.currentTarget as HTMLElement).style.background = 'rgba(136,170,255,0.15)'; }}
      onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = 'transparent'; }}
    />
  );
}

/** FacetStacked is true inside the index of a STACKED facet (FacetColumns
 *  below FACET_NARROW_PX): the index then draws itself as one horizontal
 *  strip, so it takes one row above the content instead of a third of the
 *  height, which at 375px covered the top of the content (#533 review N4). */
export const FacetStacked = createContext(false);

/** FacetColumns lays out [left at the Library's width | splitter | right]. The
 *  right column is the scroll container; `scrollRef` hands it to the caller.
 *  Below FACET_NARROW_PX of its own width (a phone, a narrow window, the rail
 *  open beside it) the index is STACKED above the content instead, full
 *  width, so the content keeps the whole width: side by side, a 375px page
 *  left the content about 150px and a confirm card 50px (#531 re-review S5).
 *  The width is the container's own, measured, so it follows the rail and
 *  the window alike; unmeasured (0, as in jsdom) is the side-by-side layout. */
export function FacetColumns({ left, children, scrollRef, testId }: {
  left: ReactNode;
  children: ReactNode;
  scrollRef?: React.Ref<HTMLDivElement>;
  testId?: string;
}) {
  const { width, onSplitterDown } = useFacetLayout();
  const box = useRef<HTMLDivElement>(null);
  const [narrow, setNarrow] = useState(false);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    const measure = () => setNarrow(el.clientWidth > 0 && el.clientWidth < FACET_NARROW_PX);
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  if (narrow) {
    return (
      <div ref={box} data-testid={testId} data-layout="stacked" style={{ ...row, flexDirection: 'column' }}>
        <div data-testid="facet-index-column" style={stackedLeft}><FacetStacked.Provider value={true}>{left}</FacetStacked.Provider></div>
        <div ref={scrollRef} data-testid="settings-scroll" style={rightCol}>{children}</div>
      </div>
    );
  }
  return (
    <div ref={box} data-testid={testId} style={row}>
      <div data-testid="facet-index-column" style={{ ...leftCol, width }}>{left}</div>
      <FacetSplitter onMouseDown={onSplitterDown} testId="facet-splitter" />
      <div ref={scrollRef} data-testid="settings-scroll" style={rightCol}>{children}</div>
    </div>
  );
}

/** Below this width of its own the facet stacks its index above the content. */
export const FACET_NARROW_PX = 640;

// flex '1 1 0' with minHeight 0, so the body is bounded by its parent and the
// right column scrolls inside it instead of the page growing to its content
// (kb/gotchas/web/ui/manage-page/flex-min-content-propagation).
const row: CSSProperties = {
  flex: '1 1 0', minHeight: 0, minWidth: 0, height: '100%',
  display: 'flex', overflow: 'hidden',
};
const leftCol: CSSProperties = {
  flexShrink: 0, overflowY: 'auto', background: '#0f0f0f',
  padding: '16px 10px', boxSizing: 'border-box',
};
// Stacked: the index across the top as one strip (the index draws itself
// horizontally under FacetStacked, scrolling sideways), ruled off from the
// content below.
const stackedLeft: CSSProperties = {
  flexShrink: 0, background: '#0f0f0f',
  padding: '6px 10px', boxSizing: 'border-box', width: '100%',
  borderBottom: '1px solid #222',
};
const rightCol: CSSProperties = {
  flex: 1, minWidth: 0, overflowY: 'auto',
};
