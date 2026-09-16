import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { PendingCreateRow } from './PendingCreateRow';
import { RepoIndexChip } from './RepoIndexChip';
import type { RepoCreateStatus } from './api';

function job(over: Partial<RepoCreateStatus> = {}): RepoCreateStatus {
  return { create_id: 'c1', name: 'kb', mode: 'subscribe', state: 'running', ...over };
}

describe('PendingCreateRow', () => {
  it('renders a running create with its chip, message and a details control', () => {
    const onOpen = vi.fn();
    render(<PendingCreateRow status={job({
      step: 'subscribe', phase: 'transfer', indeterminate: true,
      message: 'knomit: sent 3 MiB', pct: 40,
    })} onOpen={onOpen} onDismiss={vi.fn()} />);

    expect(screen.getByTestId('pending-create-kb')).toHaveAttribute('data-create-state', 'creating');
    expect(screen.getByTestId('pending-create-chip-kb')).toHaveTextContent('creating');
    expect(screen.getByTestId('pending-create-message-kb')).toHaveTextContent('knomit: sent 3 MiB');

    fireEvent.click(screen.getByTestId('pending-create-open-kb'));
    expect(onOpen).toHaveBeenCalledWith('c1');
  });

  // A RUNNING create offers no dismiss: the server refuses it (409), and a
  // control that is guaranteed to fail is worse than no control.
  it('offers no dismiss while the create is running', () => {
    render(<PendingCreateRow status={job()} onOpen={vi.fn()} onDismiss={vi.fn()} />);
    expect(screen.queryByTestId('pending-create-dismiss-kb')).toBeNull();
  });

  it('renders a failed create with its error and a dismiss that calls back', () => {
    const onDismiss = vi.fn();
    render(<PendingCreateRow
      status={job({ state: 'failed', error: 'this knowledge base is already registered locally: held by repo "kept"' })}
      onOpen={vi.fn()} onDismiss={onDismiss} />);

    expect(screen.getByTestId('pending-create-kb')).toHaveAttribute('data-create-state', 'create-failed');
    expect(screen.getByTestId('pending-create-chip-kb')).toHaveTextContent('create failed');
    expect(screen.getByTestId('pending-create-error-kb')).toHaveTextContent('already registered locally');
    // A failed row shows the reason instead of a bar: there is no progress to
    // draw, and a bar frozen at some percentage would suggest otherwise.
    expect(screen.queryByTestId('create-bar-kb')).toBeNull();
    expect(screen.queryByTestId('pending-create-open-kb')).toBeNull();

    fireEvent.click(screen.getByTestId('pending-create-dismiss-kb'));
    expect(onDismiss).toHaveBeenCalledWith('c1');
  });

  // THE BAR REFUSES TO INVENT A PERCENT. This is the incident in one
  // assertion: during the transfer the server sends no percentage and says so,
  // and a bar that filled to a made-up number there is what left the wizard
  // apparently frozen.
  it('draws an indeterminate bar with no aria percentage during transfer', () => {
    render(<PendingCreateRow status={job({ phase: 'transfer', indeterminate: true, pct: 40 })} />);
    const bar = screen.getByTestId('create-bar-indeterminate-kb');
    expect(bar).toHaveAttribute('aria-busy', 'true');
    expect(bar).not.toHaveAttribute('aria-valuenow');
    expect(screen.queryByTestId('create-bar-kb')).toBeNull();
  });

  it('draws a real percentage during indexing, which HAS one', () => {
    render(<PendingCreateRow status={job({ phase: 'index', pct: 97, message: 'indexing 40/60' })} />);
    const bar = screen.getByTestId('create-bar-kb');
    expect(bar).toHaveAttribute('aria-valuenow', '97');
    expect(screen.queryByTestId('create-bar-indeterminate-kb')).toBeNull();
    expect(screen.getByTestId('pending-create-message-kb')).toHaveTextContent('indexing 40/60');
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
