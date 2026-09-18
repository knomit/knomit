import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { PendingCreateRow, CreateBar } from './PendingCreateRow';
import { createFlag } from './useRepoCreates';
import { RepoIndexChip } from './RepoIndexChip';
import { CreateProgress } from './CreateProgress';
import type { RepoCreateStatus } from './api';

function job(over: Partial<RepoCreateStatus> = {}): RepoCreateStatus {
  return { create_id: 'c1', name: 'kb', mode: 'subscribe', state: 'running', ...over };
}

// A CREATE ROW IS A REPOSITORY ROW WITH A FLAG ON IT.
//
// The first version of this row carried its own progress bar, its own message
// line and three inline buttons, which made it the only row in the rail that
// looked like a control panel. The user's report was "all jumbled up and
// squished", reading "knomit [creating] [details] [cancel]". The row now says
// which repository it is and what state it is in, and everything else lives on
// the create's own page — the same division a repository row and its settings
// page already have.
describe('PendingCreateRow', () => {
  it('renders as a repo-shaped row: name, one flag, and no controls', () => {
    render(<PendingCreateRow status={job({
      step: 'subscribe', phase: 'transfer', indeterminate: true,
      message: 'knomit: sent 3 MiB', pct: 40,
    })} onOpen={vi.fn()} />);

    expect(screen.getByTestId('pending-create-kb')).toHaveAttribute('data-create-state', 'creating');
    expect(screen.getByTestId('pending-create-name-kb')).toHaveTextContent('kb');
    expect(screen.getByTestId('pending-create-chip-kb')).toHaveTextContent('creating');

    // NOTHING else. Not a bar, not a message, and above all not a button: a
    // row with buttons on it is the row that overflowed the rail.
    expect(screen.queryByTestId('create-bar-kb')).toBeNull();
    expect(screen.queryByTestId('create-bar-indeterminate-kb')).toBeNull();
    expect(screen.queryByTestId('pending-create-message-kb')).toBeNull();
    expect(screen.queryByTestId('pending-create-open-kb')).toBeNull();
    expect(screen.queryByTestId('pending-create-cancel-kb')).toBeNull();
    expect(screen.queryByTestId('pending-create-dismiss-kb')).toBeNull();
  });

  // THE ROW ITSELF IS THE CLICK TARGET, exactly as a repository row is. The
  // user's words: clicking the row MUST open details.
  it('opens the create when the row is clicked', () => {
    const onOpen = vi.fn();
    render(<PendingCreateRow status={job()} onOpen={onOpen} />);
    fireEvent.click(screen.getByTestId('pending-create-kb'));
    expect(onOpen).toHaveBeenCalledWith('c1');
  });

  it('flags cancelling and failed on the same one chip', () => {
    const { unmount } = render(<PendingCreateRow status={job({ state: 'cancelling' })} onOpen={vi.fn()} />);
    expect(screen.getByTestId('pending-create-chip-kb')).toHaveTextContent('cancelling');
    expect(screen.getByTestId('pending-create-kb')).toHaveAttribute('data-create-state', 'cancelling');
    unmount();

    render(<PendingCreateRow status={job({ state: 'failed', error: 'already registered locally' })} onOpen={vi.fn()} />);
    expect(screen.getByTestId('pending-create-chip-kb')).toHaveTextContent('failed');
    // The error belongs on the page, not on the row — a rail row is not where
    // a reader goes to read a sentence.
    expect(screen.queryByTestId('pending-create-error-kb')).toBeNull();
  });

  // A CANCELLED CREATE IS NOT A ROW AT ALL.
  //
  // The repository is gone and the outcome is the one the user asked for, so a
  // row saying "cancelled — dismiss" is the UI reporting their own decision
  // back to them and then asking them to acknowledge it: "what's the point, I
  // KNOW it was cancelled". The server omits these from the collection; this
  // is the same rule where the drawing happens, so a stale list cannot put the
  // row back.
  it('renders nothing for a cancelled create', () => {
    const { container } = render(
      <PendingCreateRow status={job({ state: 'cancelled' })} onOpen={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
    expect(createFlag('cancelled')).toBeNull();
  });

  it('maps every state to at most one word', () => {
    expect(createFlag('running')).toBe('creating');
    expect(createFlag('cancelling')).toBe('cancelling');
    expect(createFlag('failed')).toBe('failed');
    expect(createFlag('done')).toBe('created');
  });

  // The name yields and the flag never does, so a long repository name cannot
  // push the chip out of the rail — the "squished" half of the report.
  it('lets the name ellipsize while the flag holds its size', () => {
    render(<PendingCreateRow status={job({ name: 'a-very-long-repository-name-indeed' })} onOpen={vi.fn()} />);
    const name = screen.getByTestId('pending-create-name-a-very-long-repository-name-indeed');
    expect(name).toHaveStyle({ textOverflow: 'ellipsis', whiteSpace: 'nowrap' });
    const chip = screen.getByTestId('pending-create-chip-a-very-long-repository-name-indeed');
    expect(chip).toHaveStyle({ flexShrink: '0', whiteSpace: 'nowrap' });
  });
});

// CreateBar moved to the create's PAGE, but its contract is unchanged and is
// still the thing that refuses to invent a percentage.
describe('CreateBar', () => {
  it('draws an indeterminate bar with no aria percentage during transfer', () => {
    render(<CreateBar status={job({ phase: 'transfer', indeterminate: true, pct: 40 })} />);
    const bar = screen.getByTestId('create-bar-indeterminate-kb');
    expect(bar).toHaveAttribute('aria-busy', 'true');
    expect(bar).not.toHaveAttribute('aria-valuenow');
    expect(screen.queryByTestId('create-bar-kb')).toBeNull();
  });

  // THE WIRE CONTRACT, from the client side: the server sends indeterminate
  // WITHOUT a pct key at all, and nothing here may turn that absence into a
  // zero it then draws. The server half is
  // TestCreateStatusBody_OmitsPctWhenIndeterminate; these two fail
  // independently if the body and the components stop agreeing.
  it('draws no number at all for an indeterminate status that carries no pct', () => {
    // Exactly what the server now sends during transfer: no `pct` key.
    const status: RepoCreateStatus = {
      create_id: 'c1', name: 'kb', mode: 'subscribe', state: 'running',
      step: 'subscribe', phase: 'transfer', indeterminate: true,
      message: 'knomit: sent 3 MiB',
    };
    expect(status.pct).toBeUndefined();

    const { container } = render(<CreateBar status={status} />);
    expect(screen.getByTestId('create-bar-indeterminate-kb')).toBeInTheDocument();
    expect(screen.queryByTestId('create-bar-kb')).toBeNull();
    expect(container.textContent).not.toMatch(/\d+\s*%/);

    // And the same in the wizard's own progress view, which has its own
    // headline and could disagree with the bar.
    const progress = render(<CreateProgress status={status} />);
    expect(progress.getByTestId('create-progress-headline').textContent).toBe('knomit: sent 3 MiB');
    expect(progress.container.textContent).not.toMatch(/\d+\s*%/);
  });

  it('draws a real percentage during indexing, which HAS one', () => {
    render(<CreateBar status={job({ phase: 'index', pct: 97, message: 'indexing 40/60' })} />);
    const bar = screen.getByTestId('create-bar-kb');
    expect(bar).toHaveAttribute('aria-valuenow', '97');
    expect(screen.queryByTestId('create-bar-indeterminate-kb')).toBeNull();
  });
});

// The progress card is where a create's detail lives now. Cancelling replaces
// the headline there rather than sitting beside it: a percent and a step
// message describe work that is being abandoned, and leaving them up is what
// read as "everything is frozen, stuck in the current stage".
describe('CreateProgress cancelling', () => {
  it('replaces the progress line with Cancelling… and says what it waits for', () => {
    render(<CreateProgress status={job({ state: 'cancelling', step: 'subscribe', pct: 40, message: 'knomit: sent 3 MiB' })} />);
    expect(screen.getByTestId('create-progress-headline')).toHaveTextContent('Cancelling…');
    expect(screen.getByTestId('create-cancelling-note'))
      .toHaveTextContent('Waiting for the current step to finish, then rolling back.');
    expect(screen.getByTestId('create-progress-headline')).not.toHaveTextContent('40%');
    expect(screen.queryByTestId('create-bar-kb')).toBeNull();
    // WHERE it stopped is still true, so the step list stays.
    expect(screen.getByTestId('create-step-subscribe')).toBeInTheDocument();
  });

  // The wizard sets this the instant the button is pressed, before the 202 has
  // come back. Without it the click has no acknowledgement on a screen whose
  // progress line has already stopped moving.
  it('honours the cancelling override before the server confirms', () => {
    render(<CreateProgress status={job({ state: 'running', step: 'subscribe', pct: 40 })} cancelling />);
    expect(screen.getByTestId('create-progress-headline')).toHaveTextContent('Cancelling…');
    expect(screen.getByTestId('create-cancelling-note')).toBeInTheDocument();
  });
});

describe('RepoIndexChip', () => {
  it('shows the heal’s own counts while indexing', () => {
    render(<RepoIndexChip repo={{ index_state: 'indexing', index_done: 12, index_total: 40 }} />);
    expect(screen.getByTestId('repo-index-indexing')).toHaveTextContent('indexing 12/40');
  });

  // An uncounted heal says "indexing" and no more. "0/0" would read as a claim
  // about the repo rather than as a count not yet taken.
  it('omits counts the heal has not taken', () => {
    render(<RepoIndexChip repo={{ index_state: 'indexing', index_done: 0, index_total: 0 }} />);
    expect(screen.getByTestId('repo-index-indexing').textContent).toBe('indexing');
  });

  it('marks an index that did not finish', () => {
    render(<RepoIndexChip repo={{ index_state: 'error' }} />);
    expect(screen.getByTestId('repo-index-error')).toHaveTextContent('index error');
  });

  // Nothing for a ready repo, and nothing when the server said nothing —
  // absence is "no answer", not "ready", and both render the same way because
  // there is nothing honest to say in either case.
  it('renders nothing for ready or for silence', () => {
    const { container, rerender } = render(<RepoIndexChip repo={{ index_state: 'ready' }} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<RepoIndexChip repo={{}} />);
    expect(container).toBeEmptyDOMElement();
  });
});
