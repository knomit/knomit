import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { EdgesRail } from './EdgesRail';
import type { RefGroup } from './api';

// P1.7: EdgeRow is memoized. That memo is only live because BOTH callback hops
// are stable — EdgesRail's internal handleHop (useCallback) and the onHop it
// receives (App passes tt.hopEdge, useCallback'd on [repo, branch, dispatch]).
// An inline arrow at either hop makes the memo inert, so these tests pin the
// stability requirement, not just the presence of memo().

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

beforeEach(async () => {
  renders.typeIcon = 0;
  const { api } = await import('./api');
  (api.explain as ReturnType<typeof vi.fn>).mockResolvedValue({
    incoming: Array.from({ length: N }, (_, i) => group(i)),
    outgoing: [],
  });
});

describe('EdgesRail — EdgeRow memoization', () => {
  it('re-rendering the rail with identical props costs zero row renders', async () => {
    const onHop = vi.fn();
    const { rerender } = render(
      <EdgesRail repo="core" branch="agent/main" factPath="kb/a.md" anchorCommit="aaa" history={false} onHop={onHop} />,
    );
    await waitFor(() => expect(screen.getAllByText(/^Edge \d+$/).length).toBe(N));

    const afterFirstPaint = renders.typeIcon;
    expect(afterFirstPaint).toBeGreaterThanOrEqual(N);

    // Same props, same onHop identity — what App does on any unrelated state
    // change while a fact stays open.
    rerender(<EdgesRail repo="core" branch="agent/main" factPath="kb/a.md" anchorCommit="aaa" history={false} onHop={onHop} />);
    rerender(<EdgesRail repo="core" branch="agent/main" factPath="kb/a.md" anchorCommit="aaa" history={false} onHop={onHop} />);

    expect(screen.getAllByText(/^Edge \d+$/).length).toBe(N);
    expect(renders.typeIcon).toBe(afterFirstPaint);
  });

  it('a fresh onHop identity DOES re-render the rows — the memo depends on it', async () => {
    // The negative control for the assertion above: this is what an inline
    // `onHop={(p, c) => …}` in App would cost on every render.
    const { rerender } = render(
      <EdgesRail repo="core" branch="agent/main" factPath="kb/a.md" anchorCommit="aaa" history={false} onHop={vi.fn()} />,
    );
    await waitFor(() => expect(screen.getAllByText(/^Edge \d+$/).length).toBe(N));

    renders.typeIcon = 0;
    rerender(<EdgesRail repo="core" branch="agent/main" factPath="kb/a.md" anchorCommit="aaa" history={false} onHop={vi.fn()} />);
    expect(renders.typeIcon).toBe(N);
  });
});
