import type { Dispatch, ReactNode } from 'react';
import type { Action } from './state';
import type { Fact } from './api';
import { FactMetaLine } from './FactMetaLine';
import { displayLensPath } from './utils';

// FactBand is the fact's chrome, above the title and out of the scroll: what
// this fact is, where it lives, and every way out of it.
//
// THREE ROWS.
//   1. the values — type, kind, confidence, sources, and the mount in a lens
//   2. the path (or, once the fact's own title has scrolled away, the title)
//   3. the edges: what cites this fact, what it cites, what has the same shape,
//      and — outside the border — the version, its date, and retract.
//
// It was one row until the motif names arrived. Names are long, variable and
// worth reading, and there is no honest way to fit two of them beside a path, a
// type badge and three counters on a 935px pane; the row that used to hold the
// counters had about 120px of slack. Giving the edges a line of their own is
// the trade, and it is a real one: the header goes from about 34px to about
// 78px on every fact, whether or not it has a motif.
//
// The band exists for the SECOND state. On a long fact the title scrolls away
// by the second paragraph, taking the type, the confidence and every action
// with it — so three paragraphs in, the panel could not tell you what you were
// reading or let you act on it without scrolling back.
//
// WHY ROW 2 SWAPS RATHER THAN GROWS. The old single-row band measured whether
// the path had wrapped and let it yield only in that case, because a wrapped
// path sat exactly where the title was about to go. With rows made explicit
// that measurement has nothing left to measure: keeping both ALWAYS costs a
// fourth row. So the old rule ("the path is the one worth less while reading")
// now applies unconditionally, and the band is three rows tall in both states —
// pinned swaps row 2's contents rather than adding to them. Nothing else
// changes when pinning, which is the property that made a value vanishing
// mid-scroll read as a glitch.
export function FactBand({ fact, dispatch, lensMeta, pinned, actions, edges, filterable = true }: {
  fact: Fact;
  dispatch: Dispatch<Action>;
  lensMeta?: { repo: string; branch: string };
  /** True once the fact's own title has scrolled out of the view below. */
  pinned: boolean;
  /** The bordered edges group: everything that opens the panel below. */
  edges?: ReactNode;
  /** Version, date and retract — drawn OUTSIDE the border, because the border
   *  means "opens a panel below" and none of these do. */
  actions: ReactNode;
  /** Whether the origin badge is a filter control — see renderFact. */
  filterable?: boolean;
}) {
  return (
    <div data-testid="fact-band" style={{
      display: 'flex', flexDirection: 'column', gap: 5,
      padding: '7px 28px 8px', background: '#101014', borderBottom: '1px solid #1e222a',
      flexShrink: 0, minWidth: 0,
    }}>
      <div data-testid="fact-band-meta" style={{
        display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 8,
        minWidth: 0, fontSize: 11.5, color: '#7f8b9c',
      }}>
        <FactMetaLine fact={fact} dispatch={dispatch} lensMeta={lensMeta} filterable={filterable} />
      </div>

      {/* Row 2 always exists and is always one line, so the band is the same
          height whether or not the title has scrolled away. */}
      {pinned ? (
        <div data-testid="fact-band-title" style={{
          fontFamily: 'var(--k-font-display)', fontWeight: 600, fontSize: 12.5, color: '#e8eef6',
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0,
        }}>{fact.title || fact.path}</div>
      ) : (
        <div data-testid="fact-band-path" style={{
          fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#555',
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0,
        }}>{lensMeta ? displayLensPath(fact.path) : fact.path}</div>
      )}

      {/* Row 3. The bordered group holds only panel-openers; the version, the
          date it is read with and the retract button sit outside it. Version
          removes itself while its own history loads and is absent entirely on a
          fact with no recorded versions — outside the border it can come and go
          without leaving a hole in one. */}
      <div data-testid="fact-band-edges" style={{
        display: 'flex', alignItems: 'center', gap: 12, minWidth: 0,
      }}>
        {edges}
        <div data-testid="fact-band-actions" style={{
          marginLeft: 'auto', flex: 'none', display: 'flex', alignItems: 'center', gap: 8,
        }}>
          {actions}
        </div>
      </div>
    </div>
  );
}
