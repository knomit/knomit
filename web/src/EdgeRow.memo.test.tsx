import { describe, it, expect, vi, beforeEach } from 'vitest';
import { useState } from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { EdgeRow } from './EdgeRow';
import type { RefGroup } from './api';

// P1.7: EdgeRow is memoized, so a parent re-render costs zero row renders as
// long as the row's own props hold still.
//
// WHY THESE TESTS DRIVE EdgeRow DIRECTLY. The previous version of this file
// drove the whole EdgesRail and asserted nothing:
//   - the first case re-rendered the rail with a STABLE onHop, so EdgesRail's
//     own memo short-circuited the subtree and the rows were never reached —
//     it passed with EdgeRow's memo deleted;
//   - the second ("a fresh onHop DOES re-render the rows") passed in every
//     configuration, including with both memos removed. A tautology.
// That is not a flaw in how they were written — it is structural. EdgesRail is
// memoized, so a stable-prop re-render never reaches EdgeRow; and every prop
// that DOES get through either changes handleHop (new onHop for every row) or
// refires the fetch, whose first act is `setIncoming([])` — the rows unmount.
// There is no path through the rail on which EdgeRow's memo is observable, so
// the memo is pinned against EdgeRow itself.
//
// Renders are counted through TypeIcon, which every row renders exactly once,
// so the count is a direct read of "how many rows rendered".

const renders = vi.hoisted(() => ({ typeIcon: 0 }));

vi.mock('./icons', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./icons')>();
  return {
    ...actual,
    TypeIcon: (props: React.ComponentProps<typeof actual.TypeIcon>) => {
      renders.typeIcon += 1;
      return <actual.TypeIcon {...props} />;
    },
  };
});

vi.mock('./api', () => ({ api: { explain: vi.fn() } }));

const group = (i: number): RefGroup => ({
  path: `kb/e${i}.md`,
  title: `Edge ${i}`,
  type: 'process',
  versions: [{ commit: `commit${i}0000000`, committed_at: i }],
});

const N = 12;

// A parent that re-renders on demand while handing the rows props that do not
// move — the shape of every EdgesRail re-render the memo exists to absorb. The
// elements are freshly created each render (no element-identity bail-out doing
// the work for us); only the prop VALUES are stable.
function Rail({ groups, onHop }: { groups: RefGroup[]; onHop: (g: RefGroup, c: string) => void }) {
  const [, setTick] = useState(0);
  return (
    <div>
      <button data-testid="rerender" onClick={() => setTick(t => t + 1)}>rerender</button>
      {groups.map(g => <EdgeRow key={g.path} group={g} onHop={onHop} />)}
    </div>
  );
}

beforeEach(() => { renders.typeIcon = 0; });

describe('EdgesRail — EdgeRow memoization', () => {
  it('a parent re-render with identical row props costs zero row renders', () => {
    const groups = Array.from({ length: N }, (_, i) => group(i));
    // onHop is held stable, exactly as EdgesRail's handleHop (useCallback on
    // [onHop]) holds it. An inline arrow at either hop makes the memo inert.
    const onHop = vi.fn();
    render(<Rail groups={groups} onHop={onHop} />);

    const afterFirstPaint = renders.typeIcon;
    expect(afterFirstPaint).toBe(N);

    fireEvent.click(screen.getByTestId('rerender'));
    fireEvent.click(screen.getByTestId('rerender'));

    expect(screen.getAllByText(/^Edge \d+$/).length).toBe(N);
    // Unmemoized rows would have added 2 × N (24) renders here.
    expect(renders.typeIcon).toBe(afterFirstPaint);
  });

  it('only the row whose group actually changed re-renders', () => {
    const groups = Array.from({ length: N }, (_, i) => group(i));
    const onHop = vi.fn();
    const { rerender } = render(<Rail groups={groups} onHop={onHop} />);
    renders.typeIcon = 0;

    // One edge gains a second version — a realistic refetch result where only
    // that group's object identity moves. The memo must scope the re-render to
    // that row, not to the rail.
    const moved = [...groups];
    moved[4] = { ...group(4), versions: [...group(4).versions, { commit: 'later000000', committed_at: 99 }] };
    rerender(<Rail groups={moved} onHop={onHop} />);

    // Unmemoized this is N (12): every row re-renders because the rail did.
    expect(renders.typeIcon).toBe(1);
  });
});
