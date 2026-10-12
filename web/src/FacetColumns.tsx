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

/** FacetColumns lays out [left at the Library's width | splitter | right]. The
 *  right column is the scroll container; `scrollRef` hands it to the caller. */
export function FacetColumns({ left, children, scrollRef, testId }: {
  left: ReactNode;
  children: ReactNode;
  scrollRef?: React.Ref<HTMLDivElement>;
  testId?: string;
}) {
  const { width, onSplitterDown } = useFacetLayout();
  return (
    <div data-testid={testId} style={row}>
      <div data-testid="facet-index-column" style={{ ...leftCol, width }}>{left}</div>
      <FacetSplitter onMouseDown={onSplitterDown} testId="facet-splitter" />
      <div ref={scrollRef} data-testid="settings-scroll" style={rightCol}>{children}</div>
    </div>
  );
}

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
const rightCol: CSSProperties = {
  flex: 1, minWidth: 0, overflowY: 'auto',
};
