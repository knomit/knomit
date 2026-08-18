import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    probeOrigin: vi.fn(),
    probeInitialized: vi.fn(),
    createRepo: vi.fn(async (_b: unknown, onEvent: (e: unknown) => void) => {
      onEvent({ type: 'progress', step: 'init-git', pct: 50, message: 'x' });
      onEvent({ type: 'done', repo: { name: 'kb' } });
    }),
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'd', topics: ['invariants'] },
    ]),
    // The ontology step now shows its starting preset on arrival, so these are
    // reached by any flow that walks through it — not only by one that opens
    // the editor deliberately.
    ontologyPresetYAML: vi.fn(async () => 'id: general\nname: General\ntopics:\n  people:\n'),
    validateOntology: vi.fn(async () => ({ ok: true, id: 'general', name: 'General', topics: ['people'], rule_count: 0 })),
    ontologySchema: vi.fn(async () => []),
  },
}));

// StepOntology gates Next on its own async verification round trip, so a bare
// click straight after the step appears races that. Wait for the gate to open.
const clickNextWhenEnabled = async () => {
  await waitFor(() =>
    expect((screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
};

const probed = (over: Partial<Record<string, unknown>> = {}) => ({
  reachable: true, empty: false, auth_required: false,
  upstream_branch: 'main', branches: ['main'], ...over,
});

// Point the branch step's initialization check at one of its three answers.
// `undefined` is the UNESTABLISHED state — the absent field, exactly as the
// backend sends it — and is spelled here rather than as '' so a test that means
// "the check could not tell" cannot be misread as "the check said no".
const initializedAs = (answer: 'yes' | 'no' | undefined, detail?: string) =>
  (api.probeInitialized as ReturnType<typeof vi.fn>).mockResolvedValue(
    answer ? { initialized: answer, branch: 'main' } : { branch: 'main', detail: detail ?? 'could not read the remote' });

// Walk the branch step: press its Continue and wait for the wizard to leave it.
// Every remote flow now passes through here, because the answer to "is this
// branch already a knowledge base?" is what decides whether an ontology step
// exists at all.
const passBranchStep = async () => {
  await waitFor(() => expect(screen.getByTestId('step-branch')).toBeInTheDocument());
  fireEvent.click(screen.getByTestId('branch-check-button'));
  await waitFor(() => expect(screen.queryByTestId('step-branch')).not.toBeInTheDocument());
};

// The default createRepo: two events, ending in a repo named "kb". Restored
// before every test rather than only cleared, because mockImplementation
// installs an IMPLEMENTATION and clearAllMocks leaves it in place — so one
// test's deliberate failure became the next test's createRepo.
const okCreate = async (_b: unknown, onEvent: (e: unknown) => void) => {
  onEvent({ type: 'progress', step: 'init-git', pct: 50, message: 'x' });
  onEvent({ type: 'done', repo: { name: 'kb' } });
};

describe('CreateRepoWizard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (api.createRepo as ReturnType<typeof vi.fn>).mockImplementation(okCreate);
  });

  // THE THREE OUTCOMES, end to end through the real component.

  it('a branch that already holds a knowledge base goes to review with no ontology step', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs('yes');
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await passBranchStep();

    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({ mode: 'clone', name: 'kb' });
    // Clone takes its ontology from the remote, and the backend REFUSES one
    // supplied alongside it rather than silently dropping it.
    expect(body.ontology_preset).toBeUndefined();
    expect(body.ontology_yaml).toBeUndefined();
  });

  it('a branch that is not a knowledge base yet reaches the ontology step and submits mode initialize', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs('no');
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await passBranchStep();

    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({ mode: 'initialize', name: 'kb', ontology_preset: 'default' });
  });

  // THE THIRD OUTCOME, and the one worth the most care. A check that did not
  // complete established NOTHING, and the wizard must stop rather than route on
  // a guess: the ontology is fixed at create time and can never be changed, so
  // guessing "already one" throws away the ontology the user picked and
  // guessing "not one" writes over the ontology that governs their knowledge
  // base. Neither is recoverable.
  it('blocks on the branch step when the check does not complete, and offers a retry', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs(undefined, 'repository not found');
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

    await waitFor(() => expect(screen.getByTestId('step-branch')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('branch-check-button'));

    // It stays put, says why, and quotes the server.
    await waitFor(() => expect(screen.getByTestId('branch-blocked')).toBeInTheDocument());
    expect(screen.getByTestId('step-branch')).toBeInTheDocument();
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
    expect(screen.queryByTestId('step-review')).not.toBeInTheDocument();
    expect(screen.getByText(/repository not found/)).toBeInTheDocument();

    // And the retry genuinely retries: answer the check and the wizard moves on.
    initializedAs('no');
    fireEvent.click(screen.getByRole('button', { name: /try again/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
  });

  // The branch step's action sits in the FOOTER, beside Back and Cancel, where
  // every other step's forward action is. It used to sit in the step body,
  // stacked above the footer row — the only screen with two rows of buttons,
  // and the only one whose primary action was not where the reader had learned
  // to look. It is still not the generic Next: it runs the check.
  //
  // The check clones — shallow and single-branch, but a transfer all the same —
  // so on a large repository it takes real time. A stop that appears only while
  // something is in flight is the difference between a slow step and a frozen
  // one.
  it('runs the check from the footer, and offers a stop only while it is in flight', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    // A check that settles only when ABORTED, so the in-flight state can be
    // observed at all. It rejects on the signal rather than hanging forever,
    // because that is what the real request does — a fake that ignores the
    // signal would leave the button disabled and report a UI bug that only
    // exists in the test.
    (api.probeInitialized as ReturnType<typeof vi.fn>).mockImplementation(
      (_req: unknown, signal?: AbortSignal) => new Promise((_resolve, reject) => {
        signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
      }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await waitFor(() => expect(screen.getByTestId('step-branch')).toBeInTheDocument());

    // Nothing in flight yet: the action reads Continue and there is nothing to stop.
    expect(screen.getByTestId('branch-check-button')).toHaveTextContent(/continue/i);
    expect(screen.queryByTestId('branch-cancel-button')).not.toBeInTheDocument();
    // And no generic Next beside it — that would be a way to reach the ontology
    // step without ever establishing whether one is needed.
    expect(screen.queryByRole('button', { name: /^next$/i })).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('branch-check-button'));

    await waitFor(() =>
      expect((screen.getByTestId('branch-check-button') as HTMLButtonElement).disabled).toBe(true));
    expect(screen.getByTestId('branch-check-button')).toHaveTextContent(/checking/i);
    // "Stop checking", not "Cancel check": it stands next to Cancel, which
    // abandons the whole wizard.
    expect(screen.getByTestId('branch-cancel-button')).toHaveTextContent(/stop checking/i);

    fireEvent.click(screen.getByTestId('branch-cancel-button'));

    // A stop the reader asked for is not a failure: it leaves them on the step
    // with the action ready again, and reports nothing.
    await waitFor(() =>
      expect((screen.getByTestId('branch-check-button') as HTMLButtonElement).disabled).toBe(false));
    expect(screen.getByTestId('step-branch')).toBeInTheDocument();
    expect(screen.queryByTestId('branch-blocked')).not.toBeInTheDocument();
  });

  // The rail must not advertise a shape nobody has established. While the check
  // is unanswered there is no Ontology or Review step to promise.
  it('shows no ontology or review step in the rail while the check is unestablished', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    expect(screen.getByText('Branch')).toBeInTheDocument();
    expect(screen.queryByText('Ontology')).not.toBeInTheDocument();
    expect(screen.queryByText('Review')).not.toBeInTheDocument();
  });

  // A remote with NO branches is a dead end, not a mode: knomit never creates a
  // branch on a remote other than its own agent branch, so there is nothing to
  // cut that branch from. This case used to be "seed" and grew the LONGEST step
  // list of all; it is now the shortest, and never reaches a branch step —
  // which would have nothing to offer.
  it('stops at access for a remote with no branches', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed({ empty: true, branches: [] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    expect(screen.queryByText('Branch')).not.toBeInTheDocument();
    expect(screen.queryByText('Ontology')).not.toBeInTheDocument();
    expect(screen.queryByText('Review')).not.toBeInTheDocument();
  });

  it('shows the agreed local-only trade-off copy, verbatim and unstyled as a warning', async () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /keep it on this machine/i }));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByRole('button', { name: /next|ontology/i }));
    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));

    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(screen.getByText(/all your facts come across/i)).toBeInTheDocument();
    expect(screen.getByText(/each fact's earlier revisions/i)).toBeInTheDocument();
  });

  // Neither peer carries a badge: a badge on one makes the other read as wrong.
  it('does not label either source choice as recommended', () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    expect(screen.queryByText(/recommended/i)).not.toBeInTheDocument();
  });

  // A create that failed rolled everything back — every failure path in
  // Manager.Create calls cleanup(), dropping the local .db and the registry
  // row. The reader's first question is whether they now hold a half-made
  // repository, and the error alone does not answer it.
  it('says nothing was created when a create fails, and offers to try again', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs('no');
    (api.createRepo as ReturnType<typeof vi.fn>).mockImplementation(
      async (_b: unknown, onEvent: (e: unknown) => void) => {
        onEvent({ type: 'progress', step: 'push', pct: 70, message: 'pushing agent/host' });
        onEvent({ type: 'error', detail: 'initialize: push agent/host: authorization failed: You are not allowed to push code to this project.' });
      });
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await passBranchStep();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(screen.getByTestId('create-error')).toBeInTheDocument());
    expect(screen.getByTestId('create-error')).toHaveTextContent(/not allowed to push/i);
    expect(screen.getByTestId('create-error')).toHaveTextContent(/no repository was added/i);
    // The label must say what pressing it does now, not repeat the invitation
    // that just failed.
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^create repository$/i })).not.toBeInTheDocument();
  });

  // A failed attempt's report belongs to the attempt. Rendered by the shell
  // it followed the reader onto every other step, so the access step showed
  // "✓ Access confirmed" directly above "you are not allowed to push" — the
  // wizard contradicting itself about the same remote.
  it('leaves the failure behind when you go back to change something', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs('no');
    (api.createRepo as ReturnType<typeof vi.fn>).mockImplementation(
      async (_b: unknown, onEvent: (e: unknown) => void) => {
        onEvent({ type: 'progress', step: 'push', pct: 70, message: 'pushing agent/host' });
        onEvent({ type: 'error', detail: 'initialize: push agent/host: pre-receive hook declined' });
      });
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await passBranchStep();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
    await waitFor(() => expect(screen.getByTestId('create-error')).toBeInTheDocument());
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();

    // Back to the ontology step: neither the error nor the event log follows.
    fireEvent.click(screen.getByRole('button', { name: /^back$/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(screen.queryByTestId('create-error')).not.toBeInTheDocument();
    expect(screen.queryByTestId('create-progress')).not.toBeInTheDocument();

    // And it does not come back when the reader returns to review.
    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(screen.queryByTestId('create-error')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^create repository$/i })).toBeInTheDocument();
  });

  // The rail is the other way out of review, and it dispatches its own action.
  it('leaves the failure behind when you jump back via the step rail', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs('no');
    (api.createRepo as ReturnType<typeof vi.fn>).mockImplementation(
      async (_b: unknown, onEvent: (e: unknown) => void) => {
        onEvent({ type: 'error', detail: 'initialize: push agent/host: pre-receive hook declined' });
      });
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await passBranchStep();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
    await waitFor(() => expect(screen.getByTestId('create-error')).toBeInTheDocument());

    fireEvent.click(screen.getByTestId('wizard-step-2')); // Access
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    expect(screen.queryByTestId('create-error')).not.toBeInTheDocument();
  });

  it('omits Cancel when no onCancel is supplied', () => {
    render(<CreateRepoWizard onDone={() => {}} />);
    expect(screen.queryByRole('button', { name: /^cancel$/i })).not.toBeInTheDocument();
  });

  // Ported from the old CreateRepoForm.test.tsx. Regression: a colon-less
  // basic-auth token reads on the backend as Password with an empty
  // Username — the exact broken-credential case basic support exists to
  // avoid. Require a username before allowing Next to advance.
  it('blocks Next on the access step for basic auth with a blank username', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed({ auth_required: true }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'basic' } });
    // Password only, no username.
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 's3cret' } });

    const nextBtn = screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement;
    expect(nextBtn.disabled).toBe(true);

    // Supplying a username unblocks Next.
    fireEvent.change(screen.getByPlaceholderText('username'), { target: { value: 'alice' } });
    expect((screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement).disabled).toBe(false);
  });

  // Ported from the old CreateRepoForm.test.tsx: the old suite asserted
  // onDone was called with the repo name the server reported on its 'done'
  // event. No wizard-level test exercised that wiring; the other submit
  // tests here only check the request body.
  it('calls onDone with the created repo name for a local-only repo using the default preset', async () => {
    const onDone = vi.fn();
    render(<CreateRepoWizard onDone={onDone} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /keep it on this machine/i }));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByRole('button', { name: /next|ontology/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({ mode: 'preset', name: 'scratch', ontology_preset: 'default' });
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('kb'));
  });

  // ── The flagship workflow over HTTPS ──
  //
  // A PRIVATE repo over HTTPS is the case this whole branch exists for, and it
  // was fully broken: the anonymous probe cannot see inside, so it answers
  // {auth_required:true, empty:false}, which the wizard read as "has content"
  // and turned into mode 'clone'. The clone then substituted the hardcoded
  // default ontology, silently — the wizard never showed an ontology step for
  // the user to notice was missing.
  it('re-probes a private remote with the entered credentials and initializes it', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockResolvedValueOnce(probed({ auth_required: true, branches: [] }))
      .mockResolvedValueOnce(probed());
    initializedAs('no');
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/private.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    // The unauthenticated answer must not be treated as an answer about the
    // remote's contents — the step says why rather than moving on.
    expect(screen.getByTestId('auth-required-hint')).toBeInTheDocument();

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_x' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

    // Next re-probed WITH the credentials, and the authenticated answer let the
    // wizard reach the branch step.
    await waitFor(() => expect(screen.getByTestId('step-branch')).toBeInTheDocument());
    expect(probe.mock.calls[1][0]).toMatchObject({ auth_method: 'token', auth_token: 'ghp_x' });

    fireEvent.click(screen.getByTestId('branch-check-button'));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    // The initialization check must carry the SAME credentials. An answer
    // obtained anonymously is an answer about a different request, and this one
    // decides whether an ontology gets written.
    expect((api.probeInitialized as ReturnType<typeof vi.fn>).mock.calls[0][0])
      .toMatchObject({ url: 'https://h/private.git', auth_method: 'token', auth_token: 'ghp_x' });

    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({
      mode: 'initialize', name: 'kb', ontology_preset: 'default',
      origin: { url: 'https://h/private.git', auth_method: 'token', auth_token: 'ghp_x' },
    });
  });

  // The other HTTPS shape. A PUBLIC repo probes fine anonymously, so
  // auth_required is false — but initialize still has to PUSH its agent branch,
  // and GitHub answers 403 to an anonymous push. Gating the credential block on
  // auth_required left the user with no field to type a token into and an
  // opaque push rollback.
  it('offers credential fields for a public remote that never asked for them', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    initializedAs('no');
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/public.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    expect(screen.queryByTestId('auth-required-hint')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox')).toBeInTheDocument();
    const token = screen.getByPlaceholderText('••••••••');
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.change(token, { target: { value: 'ghp_y' } });

    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await passBranchStep();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.origin).toMatchObject({ auth_method: 'token', auth_token: 'ghp_y' });
  });

  // A remote that still refuses the supplied credentials has REFUSED them —
  // unlike the first anonymous probe, where "needs credentials" is just the
  // next question. Advancing on it would derive the mode from an answer that
  // was never given.
  it('does not advance past access while the remote still refuses the credentials', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockResolvedValue(probed({ auth_required: true, branches: [], detail: 'authentication required' }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/private.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'wrong' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

    // The refusal is reported by the outcome card, in the 'bad' tone —
    // amber is earned here because the user supplied a credential and it was
    // rejected, unlike the first probe's "needs credentials".
    await waitFor(() =>
      expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'bad'));
    expect(screen.getByTestId('outcome-detail')).toHaveTextContent('authentication required');
    expect(screen.getByTestId('step-access')).toBeInTheDocument();
    expect(screen.queryByTestId('step-review')).not.toBeInTheDocument();
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
  });

  // The explicit re-check, without committing to advancing.
  // The re-check on the access step, and what it can now discover. A remote
  // that turns out to have NO branches SHRINKS the wizard rather than growing
  // it — that case used to be "seed" and grew the longest list of all, and is
  // now a dead end the access step reports, because knomit never creates a
  // branch on a remote other than its own.
  it('re-checks access on demand and drops the branch step when the remote turns out to have none', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockResolvedValueOnce(probed({ auth_required: true, branches: [] }))
      .mockResolvedValueOnce(probed({ empty: true, branches: [] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/private.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    // source · access · branch, while what is on the remote is still unknown.
    expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(3);

    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_x' } });
    fireEvent.click(screen.getByTestId('recheck-button'));

    // Down to source · access. The re-probe's own "land on access" rule keeps
    // the user where they are rather than clamping them somewhere else.
    await waitFor(() => expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(2));
    expect(screen.getByTestId('step-access')).toBeInTheDocument();
    expect(screen.getByTestId('wizard-step-2')).toHaveAttribute('aria-current', 'step');
  });

  // The mirror case: credentials reveal a remote that DOES have branches, and
  // the wizard can go on to ask which one is the consensus branch.
  it('re-checks access on demand and keeps the branch step when the remote has branches', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockResolvedValueOnce(probed({ auth_required: true, branches: [] }))
      .mockResolvedValueOnce(probed({ branches: ['main', 'develop'] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/private.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_x' } });
    fireEvent.click(screen.getByTestId('recheck-button'));
    await waitFor(() => expect(probe).toHaveBeenCalledTimes(2));

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

    // Both branches are offered, because which one is consensus is the user's
    // call — a repo can carry the ontology on one and not the other.
    await waitFor(() => expect(screen.getByTestId('step-branch')).toBeInTheDocument());
    expect(screen.getByTestId('branch-option-main')).toBeInTheDocument();
    expect(screen.getByTestId('branch-option-develop')).toBeInTheDocument();
  });

  // ── Finding 4: the step rail ──
  it('shows no rail before the probe has said what shape the wizard is', () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    expect(screen.queryByRole('navigation', { name: /progress/i })).not.toBeInTheDocument();
  });

  it('renders a numbered rail marking the active step, and walks back through completed ones', async () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByTestId('choose-local'));

    const rail = screen.getByRole('navigation', { name: /progress/i });
    expect(rail).toBeInTheDocument();
    // Three steps, not four: local-only's name field lives on the source step
    // now, so there is no 'name' step between it and the ontology.
    expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(3);
    expect(screen.getByTestId('wizard-step-1')).toHaveAttribute('aria-current', 'step');

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(screen.getByTestId('wizard-step-2')).toHaveAttribute('aria-current', 'step');

    // A completed step is a button and goes back; the step ahead is not.
    expect(screen.getByTestId('wizard-step-3').tagName).not.toBe('BUTTON');
    fireEvent.click(screen.getByTestId('wizard-step-1'));
    await waitFor(() => expect(screen.getByTestId('create-name')).toBeInTheDocument());
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
  });

  // ── The source step is one question, disclosed ──
  //
  // Both branches used to be on screen at once as peer cards, which meant the
  // screen asked "which one?" and "fill this in" simultaneously. The segmented
  // control asks once; the pane below answers only for the chosen branch.
  describe('source step disclosure', () => {
    it('discloses the remote pane by default and only the remote pane', () => {
      render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
      expect(screen.getByTestId('source-pane-remote')).toBeInTheDocument();
      expect(screen.getByTestId('create-url')).toBeInTheDocument();
      expect(screen.queryByTestId('source-pane-local')).not.toBeInTheDocument();
      expect(screen.queryByTestId('create-name')).not.toBeInTheDocument();
      expect(screen.getByTestId('choose-remote')).toHaveAttribute('aria-pressed', 'true');
      expect(screen.getByTestId('choose-local')).toHaveAttribute('aria-pressed', 'false');
    });

    // Choosing a segment must not navigate — that is what made the old
    // CHOOSE_LOCAL jump to a step of its own. It swaps the pane in place, and
    // the footer's Next is what moves on.
    it('swaps to the local pane in place, without leaving the source step', () => {
      render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
      fireEvent.click(screen.getByTestId('choose-local'));

      expect(screen.getByTestId('step-source')).toBeInTheDocument();
      expect(screen.getByTestId('source-pane-local')).toBeInTheDocument();
      expect(screen.getByTestId('create-name')).toBeInTheDocument();
      expect(screen.queryByTestId('source-pane-remote')).not.toBeInTheDocument();
      expect(screen.queryByTestId('create-url')).not.toBeInTheDocument();
      expect(screen.getByTestId('choose-local')).toHaveAttribute('aria-pressed', 'true');
    });

    // Switching back must restore the remote pane AND keep the URL already
    // typed: a segmented control is a view of one question, not a reset.
    it('restores the remote pane and the typed URL when switching back', () => {
      render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
      fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
      fireEvent.click(screen.getByTestId('choose-local'));
      fireEvent.click(screen.getByTestId('choose-remote'));

      expect(screen.getByTestId('source-pane-remote')).toBeInTheDocument();
      expect((screen.getByTestId('create-url') as HTMLInputElement).value).toBe('https://h/r.git');
      expect(screen.queryByTestId('create-name')).not.toBeInTheDocument();
    });

    // The remote pane advances on Connect, not on a Next — a second forward
    // control there would only be a way to skip the probe.
    it('offers no Next on the remote pane', () => {
      render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
      expect(screen.queryByRole('button', { name: /^next$/i })).not.toBeInTheDocument();
      fireEvent.click(screen.getByTestId('choose-local'));
      expect(screen.getByRole('button', { name: /^next$/i })).toBeInTheDocument();
    });
  });

  // ── Also-fix: client-side name validation ──
  //
  // "My KB" used to sail through every step and come back as a 400 from POST
  // /repos — the exact 400 the deleted form's own comment called confusing.
  it('refuses a name the backend would reject, where it was typed', async () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /keep it on this machine/i }));

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'My KB' } });
    expect(screen.getByTestId('name-invalid')).toBeInTheDocument();
    expect((screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'my-kb' } });
    expect(screen.queryByTestId('name-invalid')).not.toBeInTheDocument();
    expect((screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement).disabled).toBe(false);
  });

  // ── Finding 3: the probe must be abortable ──
  //
  // The server bounds it by Cfg.Git.NetworkTimeout, but that budget is minutes
  // long; design §6 required the step stay interactive with a visible cancel.
  // This also pins that the AbortSignal is actually THREADED into the request —
  // a Cancel button wired to a fetch that ignores it is worse than none.
  it('cancels an in-flight probe without reporting a failure', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockImplementation((_b: unknown, signal?: AbortSignal) => new Promise((_res, rej) => {
      signal?.addEventListener('abort', () =>
        rej(Object.assign(new Error('aborted'), { name: 'AbortError' })));
    }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://blackhole/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('probe-cancel-button')).toBeInTheDocument());

    fireEvent.click(screen.getByTestId('probe-cancel-button'));
    await waitFor(() => expect(screen.queryByTestId('probe-cancel-button')).not.toBeInTheDocument());
    // Back to an interactive step, with no invented failure.
    expect((screen.getByTestId('probe-button') as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByTestId('step-source')).toBeInTheDocument();
    expect(screen.queryByText(/aborted/i)).not.toBeInTheDocument();
  });

  it('surfaces an unreachable remote without advancing', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      reachable: false, empty: false, auth_required: false,
      upstream_branch: '', branches: [], detail: 'no such host',
    });
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://nope/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByText(/no such host/i)).toBeInTheDocument());
    expect(screen.getByTestId('step-source')).toBeInTheDocument();
    expect(screen.queryByTestId('step-access')).not.toBeInTheDocument();
  });

  // The counterpart, and a DEAD END until the backend stopped conflating the
  // two: an SSH URL with no usable key on this machine fails inside
  // ResolveAuth, before any network call. Reported as {reachable:false} it
  // collapsed stepsFor to ['source'] — removing the access step, which is the
  // only place a credential can be entered — so the user was told the remote
  // was unreachable AND denied the one control that fixes it. There was no way
  // to create the repo at all.
  //
  // internal/repos/probe.go now reports it as {reachable:true,
  // auth_required:true} (TestProbeOrigin_UnresolvableCredentialIsAuthNotUnreachable
  // pins the producer); this pins what the user gets for it.
  it('routes an unresolvable credential to the access step instead of dead-ending on source', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed({
      auth_required: true, branches: [], upstream_branch: '',
      detail: 'resolve auth: ssh auth requires a key path',
    }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'git@github.com:user/repo.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    // A way to fix the credential — the whole point of getting here. For an SSH
    // URL that is NOT a token box (no transport can spend one): it is the auth
    // method picker, the note naming what this URL needs, the offer of the same
    // repository over HTTPS, and a way to re-run the check.
    expect(screen.getByTestId('ssh-credential-note')).toBeInTheDocument();
    expect(screen.getByTestId('use-https')).toBeInTheDocument();
    expect(screen.getByRole('combobox')).toBeInTheDocument();
    expect(screen.getByTestId('recheck-button')).toBeInTheDocument();
    // And the wrong cause is not asserted at the user.
    expect(screen.queryByText(/could not reach that remote/i)).not.toBeInTheDocument();
  });
});
