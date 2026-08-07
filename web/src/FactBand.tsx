import type { Dispatch, ReactNode } from 'react';
import type { Action } from './state';
import type { Fact } from './api';
import { FactMetaLine } from './FactMetaLine';

// FactBand is the fact's chrome, above the title and out of the scroll: what
// this fact is, where it came from, and what you can do to it.
//
// It exists for the SECOND state. On a long fact the title scrolls away by the
// second paragraph, taking the type, the confidence and every action with it —
// so three paragraphs in, the panel could not tell you what you were reading or
// let you act on it without scrolling back. Pinned, the band shows the title in
// the slot the path had, and changes nothing else.
//
// The actions live OUTSIDE the wrapping group, so a narrow panel drops the path
// to a second line and leaves them where they were. Aligned to the top for the
// same reason: on two lines they stay on the first, not centred across both.
export function FactBand({ fact, dispatch, lensMeta, pinned, actions }: {
  fact: Fact;
  dispatch: Dispatch<Action>;
  lensMeta?: { repo: string; branch: string };
  /** True once the fact's own title has scrolled out of the view below. */
  pinned: boolean;
  actions: ReactNode;
}) {
  return (
    <div data-testid="fact-band" style={{
      display: 'flex', alignItems: 'flex-start', gap: 12,
      padding: '7px 28px', background: '#101014', borderBottom: '1px solid #1e222a',
      flexShrink: 0, minWidth: 0,
    }}>
      <div data-testid="fact-band-meta" style={{
        display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 8,
        flex: '1 1 auto', minWidth: 0, fontSize: 11.5, color: '#7f8b9c',
      }}>
        {/* ONE line in both states. Pinned only swaps the last item, path for
            title — everything before it keeps its position, so scrolling does
            not reflow the band under your eye. A pinned line that dropped half
            its items looked like a different component. */}
        <FactMetaLine fact={fact} dispatch={dispatch} lensMeta={lensMeta} inline pinned={pinned} />
      </div>

      <div data-testid="fact-band-actions" style={{ flex: 'none', display: 'flex', alignItems: 'center', gap: 6 }}>
        {actions}
      </div>
    </div>
  );
}
