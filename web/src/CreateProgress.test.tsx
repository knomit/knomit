import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { CreateProgress } from './CreateProgress';
import type { RepoCreateStatus } from './api';

const status = (over: Partial<RepoCreateStatus> = {}): RepoCreateStatus => ({
  create_id: 'job1', name: 'kb', mode: 'clone', state: 'running', ...over,
});

// Read the step rows in the order they are rendered. The list is a CURSOR
// rendering — every step of the mode is drawn, marked done/current/pending —
// so the order on screen is the order the create is claimed to run in, and
// that claim is what these tests are about.
const renderedSteps = (): string[] =>
  Array.from(document.querySelectorAll('[data-testid^="create-step-"]'))
    .map(el => (el.getAttribute('data-testid') ?? '').replace('create-step-', ''));

describe('CreateProgress step order', () => {
  // THERE IS NO SYNC STEP, on any mode.
  //
  // The sync loop starts during the repo's mount walk, before the index
  // finishes, so the create has no "Activating sync" phase to narrate: after
  // register the job only observes the index, and "done" means indexed. The
  // lists mirror the emit() calls in lifecycle.go, so the whole order is
  // asserted — a stale 'sync' row, or an index drawn after it, both fail.
  it.each([
    ['preset', ['validate', 'ontology', 'init-git', 'register', 'index', 'done']],
    ['custom', ['validate', 'ontology', 'init-git', 'register', 'index', 'done']],
    ['clone', ['validate', 'clone', 'persist-origin', 'register', 'index', 'done']],
    ['initialize', ['validate', 'probe', 'ontology', 'clone', 'ontology-write', 'push', 'persist-origin', 'register', 'index', 'done']],
    ['subscribe', ['validate', 'subscribe', 'persist-origin', 'register', 'index', 'done']],
  ])('renders the server step order for mode %s', (mode, want) => {
    render(<CreateProgress status={status({ mode, step: 'register' })} />);
    expect(renderedSteps()).toEqual(want);
  });
});

describe('CreateProgress cancelled', () => {
  // A cancellation is the outcome the user ASKED for, so the card says what
  // the world looks like now rather than drawing stalled progress or an error
  // to read. The step list and the bar would both describe work that is no
  // longer happening.
  it('renders the cancelled card and nothing else', () => {
    render(<CreateProgress status={status({ state: 'cancelled', step: 'clone', pct: 40 })} />);
    expect(screen.getByTestId('create-cancelled')).toHaveTextContent(
      'Create cancelled. No repository was added.');
    expect(screen.queryByTestId('create-progress')).not.toBeInTheDocument();
    expect(renderedSteps()).toEqual([]);
  });

  it('does not draw a cancelled create as a failure', () => {
    render(<CreateProgress status={status({ state: 'cancelled' })} />);
    // No error line: nothing went wrong, so there is nothing to read and
    // nothing to retry. Rendering it beside 'failed' is the confusion this
    // separate state exists to prevent.
    expect(screen.queryByText(/failed/i)).not.toBeInTheDocument();
  });
});
