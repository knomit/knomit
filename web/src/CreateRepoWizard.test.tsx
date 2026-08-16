import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    probeOrigin: vi.fn(),
    createRepo: vi.fn(async (_b: unknown, onEvent: (e: unknown) => void) => {
      onEvent({ type: 'progress', step: 'init-git', pct: 50, message: 'x' });
      onEvent({ type: 'done', repo: { name: 'kb' } });
    }),
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'd', topics: ['invariants'] },
    ]),
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

describe('CreateRepoWizard', () => {
  beforeEach(() => vi.clearAllMocks());

  it('a populated remote goes straight to review with no ontology step', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));

    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
  });

  it('an empty remote reaches the ontology step and submits mode seed', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed({ empty: true, branches: [] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /next|ontology/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({ mode: 'seed', name: 'kb', ontology_preset: 'default' });
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

  // ── Finding 1: the flagship workflow over HTTPS ──
  //
  // A PRIVATE empty repo over HTTPS is the case this whole branch exists for,
  // and it was fully broken: the anonymous probe cannot see inside, so it
  // answers {auth_required:true, empty:false}, stepsFor read that as "has
  // content", and createBodyFor emitted mode 'clone'. The clone then seeded the
  // hardcoded default ontology and pushed nothing — the original bug, still
  // reachable, and now silent, because the wizard never showed an ontology step
  // for the user to notice was missing.
  it('re-probes a private empty remote with the entered credentials and seeds it', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockResolvedValueOnce(probed({ auth_required: true, branches: [] }))
      .mockResolvedValueOnce(probed({ empty: true, branches: [] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/private.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());

    // The unauthenticated answer must not be treated as an answer about
    // emptiness — no ontology step is offered yet, and the step says why.
    expect(screen.getByTestId('auth-required-hint')).toBeInTheDocument();

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_x' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

    // Next re-probed WITH the credentials, and the authenticated answer put the
    // ontology step into the list.
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(probe.mock.calls[1][0]).toMatchObject({ auth_method: 'token', auth_token: 'ghp_x' });

    await clickNextWhenEnabled();
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({
      mode: 'seed', name: 'kb', ontology_preset: 'default',
      origin: { url: 'https://h/private.git', auth_method: 'token', auth_token: 'ghp_x' },
    });
  });

  // The other HTTPS shape. A PUBLIC empty repo probes fine anonymously, so
  // auth_required is false — but the seed still has to PUSH, and GitHub answers
  // 403 to an anonymous push. Gating the credential block on auth_required left
  // the user with no field to type a token into and an opaque
  // "seed: push main: …" rollback.
  it('offers credential fields for a public empty remote that never asked for them', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed({ empty: true, branches: [] }));
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

    await waitFor(() => expect(screen.getByTestId('access-error')).toBeInTheDocument());
    expect(screen.getByTestId('step-access')).toBeInTheDocument();
    expect(screen.queryByTestId('step-review')).not.toBeInTheDocument();
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
  });

  // The explicit re-check, without committing to advancing.
  it('re-checks access on demand and gains an ontology step when the remote turns out empty', async () => {
    const probe = api.probeOrigin as ReturnType<typeof vi.fn>;
    probe.mockResolvedValueOnce(probed({ auth_required: true, branches: [] }))
      .mockResolvedValueOnce(probed({ empty: true, branches: [] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/private.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));
    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    // Three steps while emptiness is unknown: source, access, review.
    expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(3);

    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_x' } });
    fireEvent.click(screen.getByTestId('recheck-button'));

    // The list gains 'ontology', and the re-probe's own "land on access" rule
    // keeps the user where they are rather than clamping them somewhere else.
    await waitFor(() => expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(4));
    expect(screen.getByTestId('step-access')).toBeInTheDocument();
    expect(screen.getByTestId('wizard-step-2')).toHaveAttribute('aria-current', 'step');
  });

  // ── Finding 4: the step rail ──
  it('shows no rail before the probe has said what shape the wizard is', () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    expect(screen.queryByRole('navigation', { name: /progress/i })).not.toBeInTheDocument();
  });

  it('renders a numbered rail marking the active step, and walks back through completed ones', async () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /keep it on this machine/i }));

    const rail = screen.getByRole('navigation', { name: /progress/i });
    expect(rail).toBeInTheDocument();
    expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(4);
    expect(screen.getByTestId('wizard-step-2')).toHaveAttribute('aria-current', 'step');

    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(screen.getByTestId('wizard-step-3')).toHaveAttribute('aria-current', 'step');

    // A completed step is a button and goes back; the step ahead is not.
    expect(screen.getByTestId('wizard-step-4').tagName).not.toBe('BUTTON');
    fireEvent.click(screen.getByTestId('wizard-step-2'));
    await waitFor(() => expect(screen.getByTestId('create-name')).toBeInTheDocument());
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
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
    // The credential fields — the whole point of getting here.
    expect(screen.getByPlaceholderText('••••••••')).toBeInTheDocument();
    expect(screen.getByTestId('recheck-button')).toBeInTheDocument();
    // And the wrong cause is not asserted at the user.
    expect(screen.queryByText(/could not reach that remote/i)).not.toBeInTheDocument();
  });
});
