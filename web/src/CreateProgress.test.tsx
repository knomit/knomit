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
  // THE INDEX IS NARRATED BEFORE SYNC, on every remote mode.
  //
  // This is the reported bug, and it is an ORDERING bug rather than a wording
  // one: ActivateSync runs a synchronous reconcile that takes the branch lock
  // the background index heal already holds, so with sync emitted first the
  // job sat on "Activating sync" for the whole of the index. A user who looked
  // at the repo saw it already indexing while the wizard said it was doing
  // something else. lifecycle.go now emits the index first; this list mirrors
  // the server, and a mirror that disagrees puts the lie back on screen.
  it.each(['clone', 'initialize', 'subscribe'])('puts index before sync for mode %s', mode => {
    render(<CreateProgress status={status({ mode, step: 'register' })} />);
    const steps = renderedSteps();
    expect(steps).toContain('index');
    expect(steps).toContain('sync');
    expect(steps.indexOf('index')).toBeLessThan(steps.indexOf('sync'));
    // And both sit after the repo is registered and before it is called done,
    // so this is not satisfied by some unrelated reshuffle of the list.
    expect(steps.indexOf('register')).toBeLessThan(steps.indexOf('index'));
    expect(steps.indexOf('sync')).toBeLessThan(steps.indexOf('done'));
  });

  it('leaves the local modes alone: they have no sync step to order', () => {
    for (const mode of ['preset', 'custom']) {
      const { unmount } = render(<CreateProgress status={status({ mode, step: 'register' })} />);
      expect(renderedSteps()).not.toContain('sync');
      unmount();
    }
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
