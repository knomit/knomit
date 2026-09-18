import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api } from './api';

vi.mock('./useRepoCreates', () => ({ refreshRepoCreates: vi.fn(async () => {}) }));

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    probeOrigin: vi.fn(),
    probeInitialized: vi.fn(),
    createRepo: vi.fn(),
    cancelRepoCreate: vi.fn(),
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
    ]),
    ontologyPresetYAML: vi.fn(async () => 'id: general\nname: General\ntopics:\n  people:\n'),
    validateOntology: vi.fn(async () => ({ ok: true, id: 'general', name: 'General', topics: ['people'], rule_count: 0 })),
    ontologySchema: vi.fn(async () => []),
  },
}));

const mock = (fn: unknown) => fn as ReturnType<typeof vi.fn>;

const status = (over: Record<string, unknown> = {}) => ({
  create_id: 'job1', name: 'scratch', mode: 'preset', state: 'running', ...over,
});

// Walk the local-only flow to the review step and press Create. Local-only is
// the shortest route to a running create, and nothing about cancelling depends
// on which mode produced the job.
const startLocalCreate = async () => {
  fireEvent.click(screen.getByRole('radio', { name: /keep it on this machine/i }));
  fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
  fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
  await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
  // StepOntology gates Next on its own async verification round trip.
  await waitFor(() =>
    expect((screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
  await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
  fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
};

// A create that reports one running status and then PARKS, so the test can
// look at the wizard mid-flight — which is the only window the cancel control
// is supposed to exist in. `settle` ends it with whatever terminal status the
// test wants, standing in for the poll that observes the server's answer.
function parkedCreate() {
  let release: (s: Record<string, unknown>) => void = () => {};
  // The wizard's own onStatus callback, captured so a test can deliver a poll
  // result OUT OF BAND — which is exactly what a request already in flight when
  // the cancel returned does.
  let report: (s: unknown) => void = () => {};
  const parked = new Promise<Record<string, unknown>>(res => { release = res; });
  const impl = async (_b: unknown, onStatus: (s: unknown) => void) => {
    report = onStatus;
    onStatus(status({ step: 'init-git', pct: 40, message: 'creating' }));
    const final = await parked;
    onStatus(final);
    return final;
  };
  return {
    impl,
    settle: (over: Record<string, unknown>) => release(status(over)),
    deliver: (over: Record<string, unknown>) => act(() => { report(status(over)); }),
  };
}

describe('CreateRepoWizard cancel', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  // THE CONTROL EXISTS ONLY WHILE THE CREATE IS RUNNING.
  //
  // Driven by the REPORTED state, not by the wizard's own `creating` flag: the
  // server answers 409 for a job that already failed or was cancelled, so a
  // button offered in those states is one that exists to be refused — the dead
  // control this wizard has removed elsewhere.
  it('offers no cancel before a create starts', async () => {
    const { impl } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.click(screen.getByRole('radio', { name: /keep it on this machine/i }));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
    expect(screen.queryByTestId('create-cancel-button')).not.toBeInTheDocument();
  });

  it('shows the cancel button while running and withdraws it once the job is terminal', async () => {
    const { impl, settle } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());

    settle({ state: 'done', step: 'done', pct: 100, repo: { name: 'scratch' } });
    await waitFor(() =>
      expect(screen.queryByTestId('create-cancel-button')).not.toBeInTheDocument());
  });

  it('presses through to api.cancelRepoCreate with the running job id', async () => {
    const { impl } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    mock(api.cancelRepoCreate).mockResolvedValue(status());
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('create-cancel-button'));

    await waitFor(() => expect(api.cancelRepoCreate).toHaveBeenCalledWith('job1'));
  });

  // CANCELLING DOES NOT STOP THE POLLING, and this is the load-bearing half.
  //
  // Cancel is ACCEPTED (202), not performed — a running create stops at its
  // next step boundary — so the job's terminal state is still something the
  // server reaches and the existing poll reports. A wizard that tore down its
  // observer on the 202 would show a job frozen wherever it happened to be.
  it('keeps observing the create after the cancel is accepted', async () => {
    const { impl, settle } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    // The 202's body still says `running`: the work has not stopped yet.
    mock(api.cancelRepoCreate).mockResolvedValue(status({ step: 'init-git', pct: 40 }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('create-cancel-button'));
    await waitFor(() => expect(api.cancelRepoCreate).toHaveBeenCalled());

    // Still watching: the progress card is up and the create has not been
    // declared over on the strength of the 202 alone.
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();
    expect(screen.queryByTestId('create-cancelled')).not.toBeInTheDocument();

    // ...and the poll that follows carries it to its real end.
    settle({ state: 'cancelled', step: 'init-git' });
    await waitFor(() => expect(screen.getByTestId('create-cancelled')).toBeInTheDocument());
  });

  // onDone NAVIGATES THE APP INTO A REPOSITORY. A cancelled create left none,
  // so calling it here would drop the user into a repo that does not exist,
  // immediately after they asked for it not to be made.
  it('renders the cancelled card and does NOT call onDone', async () => {
    const onDone = vi.fn();
    const { impl, settle } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    mock(api.cancelRepoCreate).mockResolvedValue(status());
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('create-cancel-button'));
    settle({ state: 'cancelled', step: 'init-git' });

    await waitFor(() => expect(screen.getByTestId('create-cancelled')).toHaveTextContent(
      'Create cancelled. No repository was added.'));
    expect(onDone).not.toHaveBeenCalled();
    // Not an error either: the user got what they asked for.
    expect(screen.queryByTestId('create-error')).not.toBeInTheDocument();
  });

  it('still calls onDone when the create was never cancelled', async () => {
    const onDone = vi.fn();
    const { impl, settle } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    await startLocalCreate();

    settle({ state: 'done', step: 'done', pct: 100, repo: { name: 'scratch' } });
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('scratch'));
  });

  // THE CLICK IS ACKNOWLEDGED IMMEDIATELY, before the server has answered.
  //
  // This is the whole of the reported bug. The user pressed Cancel and the
  // screen went on showing the same step and the same percent — "everything is
  // frozen, stuck in the current stage" — because nothing changed until the
  // 202 came back, and on a slow step that is a long time to wonder whether
  // the click registered at all.
  it('says Cancelling… the moment the button is pressed, before the 202 lands', async () => {
    const { impl } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    // A cancel request that has NOT come back yet.
    let release: (s: unknown) => void = () => {};
    mock(api.cancelRepoCreate).mockReturnValue(new Promise(res => { release = res; }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());
    expect(screen.getByTestId('create-cancel-button')).toHaveTextContent('Cancel create');

    fireEvent.click(screen.getByTestId('create-cancel-button'));

    // Disabled, relabelled, and saying what the wait is for — with the server
    // still to answer.
    await waitFor(() =>
      expect(screen.getByTestId('create-cancel-button')).toHaveTextContent('Cancelling…'));
    expect(screen.getByTestId('create-cancel-button')).toBeDisabled();
    expect(screen.getByTestId('create-cancelling-note'))
      .toHaveTextContent('Waiting for the current step to finish, then rolling back.');
    // The abandoned progress line is gone rather than left up to look stuck.
    expect(screen.getByTestId('create-progress-headline')).toHaveTextContent('Cancelling…');
    // ONE STATUS ONLY. The wizard's own 'creating' flag is still true through
    // a cancel, so the primary button went on reading "Creating…" right beside
    // this one — a footer claiming both at once is the contradictory status
    // this screen exists to avoid. Absent, not disabled: the cancel IS the action.
    expect(screen.queryByRole('button', { name: /^Creating/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /create repository/i })).toBeNull();

    // And it STAYS saying so once the 202 lands reading 'cancelling', rather
    // than flicking back to "Cancel create" between the two sources.
    await act(async () => { release(status({ state: 'cancelling' })); });
    expect(screen.getByTestId('create-cancel-button')).toHaveTextContent('Cancelling…');
    expect(screen.getByTestId('create-cancel-button')).toBeDisabled();
  });

  // A TERMINAL STATE NEVER GOES BACK TO RUNNING.
  //
  // Two writers race here and are not ordered with respect to each other: the
  // 202 body the cancel adopts, and a poll request that was ALREADY IN FLIGHT
  // when it returned. That poll carries the snapshot the server held before the
  // cancel was recorded, so without the guard the card flips from "Create
  // cancelled" back to a progress bar for up to one poll interval — telling the
  // user the thing they just stopped is still going.
  it('ignores a stale running poll that lands after the cancel', async () => {
    const onDone = vi.fn();
    const { impl, settle, deliver } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    // The done-job case: the delete is synchronous, so the 202 already reads
    // cancelled.
    mock(api.cancelRepoCreate).mockResolvedValue(status({ state: 'cancelled' }));
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('create-cancel-button'));
    await waitFor(() => expect(screen.getByTestId('create-cancelled')).toBeInTheDocument());

    // The straggler arrives, still describing a running create.
    deliver({ state: 'running', step: 'init-git', pct: 40, message: 'creating' });

    expect(screen.getByTestId('create-cancelled')).toBeInTheDocument();
    expect(screen.queryByTestId('create-progress')).not.toBeInTheDocument();
    // And the control does not come back to be pressed a second time.
    expect(screen.queryByTestId('create-cancel-button')).not.toBeInTheDocument();

    // The real terminal status still lands normally.
    settle({ state: 'cancelled', step: 'init-git' });
    await waitFor(() => expect(screen.getByTestId('create-cancelled')).toBeInTheDocument());
    expect(onDone).not.toHaveBeenCalled();
  });

  // A REFUSED CANCEL IS THE ONE OUTCOME HERE THAT MUST NOT REASSURE.
  //
  // Every other report on this screen ends "No repository was added". If the
  // cancel itself failed, a repo very possibly WAS added, and the reader has
  // to go and look — so this gets its own message rather than createErr's.
  it('reports a refused cancel without claiming nothing was created', async () => {
    const { impl } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    mock(api.cancelRepoCreate).mockRejectedValue(new Error('cancel create → 409 already finished'));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('create-cancel-button'));

    await waitFor(() => expect(screen.getByTestId('create-cancel-error')).toBeInTheDocument());
    expect(screen.getByTestId('create-cancel-error')).toHaveTextContent(/409 already finished/);
    expect(screen.getByTestId('create-cancel-error')).toHaveTextContent(/was not cancelled/i);
    expect(screen.queryByTestId('create-cancelled')).not.toBeInTheDocument();
  });

  // NO NAVIGATION ON A NON-TERMINAL FINAL STATE.
  //
  // The guard in handleCreate used to be the NEGATIVE one — anything that was
  // not 'failed' navigated — and that is what put the user on the settings
  // page of a repository that had never been created. The upstream cause (the
  // poll loop stopping on 'cancelling') is fixed and tested in
  // api.createCancel.test.ts; this is the second line of defence, and it is
  // worth having on its own: handleCreate must not treat 'not failed' as
  // 'exists'. So this feeds it exactly what the broken loop produced — a
  // create resolving while still cancelling.
  it('never navigates when the create resolves in a non-terminal state', async () => {
    const onDone = vi.fn();
    mock(api.createRepo).mockImplementation(async (_b: unknown, onStatus: (s: unknown) => void) => {
      onStatus(status({ step: 'subscribe', pct: 10 }));
      const final = status({ state: 'cancelling', step: 'subscribe' });
      onStatus(final);
      return final;
    });
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancelling-note')).toBeInTheDocument());
    expect(onDone).not.toHaveBeenCalled();
    // Still on review, showing the create — not navigated anywhere.
    expect(screen.getByTestId('step-review')).toBeInTheDocument();
  });

  // And the terminal cancel still ends on the cancelled card with no
  // navigation.
  it('ends a cancelled create on the cancelled card without navigating', async () => {
    const onDone = vi.fn();
    mock(api.createRepo).mockImplementation(async (_b: unknown, onStatus: (s: unknown) => void) => {
      onStatus(status({ step: 'subscribe', pct: 10 }));
      onStatus(status({ state: 'cancelling', step: 'subscribe' }));
      const final = status({ state: 'cancelled', step: 'subscribe' });
      onStatus(final);
      return final;
    });
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    await startLocalCreate();

    await waitFor(() => expect(screen.getByTestId('create-cancelled')).toBeInTheDocument());
    expect(onDone).not.toHaveBeenCalled();
  });

  // A create that genuinely finishes still navigates — the positive test that
  // stops the fix above from being "never call onDone".
  it('still navigates on a real done', async () => {
    const onDone = vi.fn();
    const { impl, settle } = parkedCreate();
    mock(api.createRepo).mockImplementation(impl);
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    await startLocalCreate();
    settle({ state: 'done', step: 'done', pct: 100, repo: { name: 'scratch' } });
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('scratch'));
  });

});
