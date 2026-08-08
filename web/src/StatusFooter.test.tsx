// The footer took over the two things in the top bar you cannot act on: the
// commit, and (in lens context) the repo writes land in. The top bar is now
// controls only — everything there opens something — and the footer is the
// readout rail.
//
// For the commit this is a net GAIN in visibility. In lens context the top bar
// rendered it only when a fact was open AND the view was anchored, so during
// ordinary reading there was no commit on screen at all.

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { StatusFooter } from './StatusFooter';
import { init } from './state';
import type { AppState, AsOf } from './state';
import type { Lens } from './api';

const lens: Lens = { name: 'all', write: 'test', reads: [{ repo: 'core' }, { repo: 'docs' }] };

const repoState: AppState = {
  ...init,
  repo: 'alpha', branch: 'agent/test', headCommit: 'head0001234abcdef',
  context: { kind: 'repo', repo: 'alpha' },
};
const lensState: AppState = {
  ...init,
  repo: 'test', branch: 'agent/test', headCommit: 'head0001234abcdef',
  context: { kind: 'lens', name: 'all' }, lens,
};

const footer = () => screen.getByTestId('status-footer');

describe('StatusFooter — the commit', () => {
  it('shows the live head, abbreviated', () => {
    render(<StatusFooter state={repoState} />);
    expect(screen.getByTestId('footer-commit')).toHaveTextContent('head000');
  });

  it('adds no second hash when anchored — the mode descriptor is already the commit', () => {
    // Only the LIVE descriptor is a bare word ("HEAD"); history renders the
    // commit itself and diff renders from..to. Printing head alongside them
    // put the same seven characters on the rail twice.
    render(<StatusFooter state={{ ...repoState, asOf: { mode: 'history', commit: 'sc123456abcd' } }} />);
    expect(screen.queryByTestId('footer-commit')).toBeNull();
    expect(footer().textContent).toContain('sc12345');
  });

  it('adds no second hash in diff mode either', () => {
    render(<StatusFooter state={{ ...repoState, asOf: { mode: 'diff', from: 'aaa1111bbbb', to: 'bbb2222cccc' } }} />);
    expect(screen.queryByTestId('footer-commit')).toBeNull();
  });

  it('never prints one commit twice, in any mode', () => {
    // The invariant the two cases above are instances of.
    const modes: AsOf[] = [
      { mode: 'live' },
      { mode: 'history', commit: 'sc123456abcd' },
      { mode: 'diff', from: 'aaa1111bbbb', to: 'bbb2222cccc' },
    ];
    for (const asOf of modes) {
      const { unmount } = render(<StatusFooter state={{ ...repoState, asOf }} />);
      const text = footer().textContent!;
      for (const hash of ['head000', 'sc12345', 'aaa1111', 'bbb2222']) {
        expect(text.split(hash).length - 1, `${hash} in ${asOf.mode}`).toBeLessThan(2);
      }
      unmount();
    }
  });

  it('renders nothing rather than an empty slot when there is no commit yet', () => {
    // headCommit is '' — not null — until the first status poll resolves, which
    // is why the render guard is a truthiness check and not a null check.
    render(<StatusFooter state={{ ...repoState, headCommit: '' }} />);
    expect(screen.queryByTestId('footer-commit')).toBeNull();
  });

  it('shows a commit in lens context too, with no fact open', () => {
    // The case the top bar could not: it gated on state.factPath AND on being
    // anchored, so a live lens read showed no commit anywhere.
    render(<StatusFooter state={{ ...lensState, factPath: null }} />);
    expect(screen.getByTestId('footer-commit')).toHaveTextContent('head000');
  });
});

describe('StatusFooter — the write target', () => {
  it('names the repo writes land in when reading through a lens', () => {
    render(<StatusFooter state={lensState} />);
    expect(screen.getByTestId('footer-writes')).toHaveTextContent('test');
  });

  it('stays out of repo context, where there is no separate target', () => {
    // You write to the repo you are browsing; a pill saying so is noise.
    render(<StatusFooter state={repoState} />);
    expect(screen.queryByTestId('footer-writes')).toBeNull();
  });

  it('keeps its box on the way down instead of dissolving into the rail', () => {
    // Everything else here is a muted readout you glance past. This one is the
    // last thing between the reader and a fact landing in the wrong repo, so it
    // stays the brightest element on the line.
    render(<StatusFooter state={lensState} />);
    const writes = screen.getByTestId('footer-writes');
    expect(writes.style.border).not.toBe('');
    expect(writes.style.background).not.toBe('');
  });

  it('says where writes go even while reading history, when they cannot happen', () => {
    // Read-only is temporary and the reader returns to now with `h`. Dropping
    // the target here would make it flicker on every scrub.
    render(<StatusFooter state={{ ...lensState, asOf: { mode: 'history', commit: 'sc123456abcd' } }} />);
    expect(screen.getByTestId('footer-writes')).toHaveTextContent('test');
  });
});

describe('StatusFooter — what it already did', () => {
  it('still reports the mode and the build', () => {
    render(<StatusFooter state={repoState} version="0.5.1" />);
    expect(screen.getByTestId('footer-mode')).toBeTruthy();
    expect(footer().textContent).toContain('0.5.1');
  });
});

// The hint was a three-state `searchHint` ('open' | 'close' | null) back when
// the dashboard had a search box of its own that `/` opened and `esc` put away.
// That box is gone — there is one field now, always on screen while live — so
// the 'close' arm became unreachable and the question is a boolean.
describe('StatusFooter — the search key', () => {
  it('advertises `/` while live, and nothing while anchored', () => {
    const { rerender } = render(<StatusFooter state={repoState} searchKey={true} />);
    expect(footer().textContent).toContain('search');

    rerender(<StatusFooter state={repoState} searchKey={false} />);
    expect(footer().textContent).not.toContain('search');
  });

  it('never offers `esc close` — there is nothing to close', () => {
    render(<StatusFooter state={repoState} searchKey={true} />);
    expect(footer().textContent).not.toContain('close');
  });
});

// `h now` used to be advertised unconditionally while the action behind it only
// does anything in history mode — so the one shortcut on screen looked broken
// to everyone who was not time-travelling. A hint is a promise; it is the
// inverse of the search key now, and for the same reason.
describe('StatusFooter — the history key', () => {
  it('advertises `h` while anchored, and nothing while live', () => {
    const { rerender } = render(<StatusFooter state={repoState} historyKey={true} />);
    expect(footer().textContent).toContain('now');

    rerender(<StatusFooter state={repoState} historyKey={false} />);
    expect(footer().textContent).not.toContain('now');
  });

  it('offers neither key when both are withheld, as in Manage', () => {
    // Manage has no time axis to return from and no filter field to focus.
    render(<StatusFooter state={repoState} searchKey={false} historyKey={false} />);
    expect(footer().textContent).not.toContain('now');
    expect(footer().textContent).not.toContain('search');
  });
});
