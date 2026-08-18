import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api, type ProbeResult } from './api';

// A stand-in git host, driven by the ProbeResult shapes the real one can
// produce. ProbeOrigin reports every outcome as a normal 200 — unreachable and
// auth-required are RESULTS, not errors (internal/repos/probe.go) — so a fake
// that returns these five shapes covers the whole surface the wizard reacts to,
// without a network, a server, or a real remote.
//
// These tests exist because a break got all the way to a human: typing a token
// against an SSH URL produced an error the backend classified as unreachable,
// stepsFor collapsed to ['source'], and the wizard threw the user back to the
// first screen — taking the access step, the only place a credential can be
// corrected, with it. Nothing pinned "the wizard never rewinds off access".
const HOST = {
  empty: (): ProbeResult => ({
    reachable: true, empty: true, auth_required: false, upstream_branch: 'main', branches: [],
  }),
  populated: (): ProbeResult => ({
    reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main', 'draft'],
  }),
  needsCredentials: (): ProbeResult => ({
    reachable: true, empty: false, auth_required: true, upstream_branch: '', branches: [],
    detail: 'ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]',
  }),
  // What the backend returns for a credential that does not fit the URL's
  // transport, BEFORE the classifyProbeError fix that made it auth-required.
  // Kept as a case in its own right: any error the server cannot classify
  // arrives in exactly this shape, and the wizard must survive all of them.
  unclassifiedFailure: (): ProbeResult => ({
    reachable: false, empty: false, auth_required: false, upstream_branch: '', branches: [],
    detail: 'invalid auth method',
  }),
  unreachable: (): ProbeResult => ({
    reachable: false, empty: false, auth_required: false, upstream_branch: '', branches: [],
    detail: 'dial tcp: lookup nope.example: no such host',
  }),
};

vi.mock('./api', () => ({
  api: {
    probeOrigin: vi.fn(),
    createRepo: vi.fn(),
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
    ]),
    ontologyPresetYAML: vi.fn(async () => 'id: general\nname: General\ntopics:\n  people:\n'),
    validateOntology: vi.fn(async () => ({ ok: true, id: 'x', name: 'X', topics: ['a'], rule_count: 1 })),
    ontologySchema: vi.fn(async () => []),
  },
}));

const probeOrigin = () => api.probeOrigin as ReturnType<typeof vi.fn>;

// Walks source → access for a URL, with whatever the fake host answers first.
async function reachAccess(url: string, first: ProbeResult) {
  probeOrigin().mockResolvedValueOnce(first);
  render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
  fireEvent.change(screen.getByTestId('create-url'), { target: { value: url } });
  fireEvent.click(screen.getByTestId('probe-button'));
  await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
}

describe('the wizard against a fake git host', () => {
  // mockReset, not just clearAllMocks: mockResolvedValueOnce queues an
  // IMPLEMENTATION, which clearAllMocks leaves in place. One test that fails
  // before consuming its queued answer would otherwise hand that answer to the
  // next test, and the cascade reads like three unrelated breaks.
  beforeEach(() => {
    vi.clearAllMocks();
    probeOrigin().mockReset();
  });

  describe('every reachable outcome reports itself', () => {
    // A remote with no branches is where the wizard STOPS. knomit never
    // creates a branch on a remote other than its own agent branch, so there
    // is nothing to cut that branch from — the user has to give the remote a
    // `main` first, and one commit is enough.
    //
    // This used to be the "seed" path, and it grew the LONGEST step list of
    // all (source · access · ontology · review). It is now the shortest remote
    // list there is, because the create it led to pushed the remote's default
    // branch and every host protects that on a new project.
    it('a remote with no branches stops the wizard at access', async () => {
      await reachAccess('https://h/new.git', HOST.empty());
      expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(2);
      expect(screen.queryByText('Branch')).not.toBeInTheDocument();
      expect(screen.queryByText('Ontology')).not.toBeInTheDocument();
    });

    // The card has to SAY the wizard stopped, and say what fixes it. It used
    // to say the opposite — that knomit would write the first commit itself
    // and the ontology step was next — which was true of the deleted "seed"
    // mode and is now a promise the step list cannot keep: the rail shows two
    // steps, there is no ontology step, and nothing writes a first commit.
    it('a remote with no branches asks for one instead of promising a create', async () => {
      await reachAccess('https://h/new.git', HOST.empty());
      const card = screen.getByTestId('outcome-card');
      expect(card).toHaveTextContent(/main/);
      expect(card).toHaveTextContent(/one commit is enough/i);
      expect(card).not.toHaveTextContent(/first commit itself/i);
      expect(card).not.toHaveTextContent(/ontology next/i);
    });

    // A Next on a step that is LAST in the derived list has nowhere to go:
    // NEXT clamps to the index it is already on, so the button is enabled,
    // pressing it does nothing, and the wizard gives no reason. This was
    // reached by a human — an empty GitLab project, a blue Next, no response.
    // The rule is derived from the same list that does the routing, so it
    // covers any future terminal step rather than the empty case alone.
    it('offers no Next on a step nothing follows', async () => {
      await reachAccess('https://h/new.git', HOST.empty());
      expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(2);
      expect(screen.queryByRole('button', { name: 'Next' })).not.toBeInTheDocument();
    });

    it('a populated remote names its branches, and offers the branch step', async () => {
      await reachAccess('https://h/existing.git', HOST.populated());
      expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'good');
      expect(screen.getByTestId('probe-branches')).toHaveTextContent('draft');
      // And it must not PROMISE the shape of the rest of the wizard. This card
      // used to say knomit would "adopt the ontology already in it — so there
      // is no ontology step for this case", which is the deleted clone-only
      // flow: whether an ontology step exists is decided by the branch check on
      // the NEXT step, and for a repository with a README and no .knomit/ the
      // answer is that one does.
      const card = screen.getByTestId('outcome-card');
      expect(card).not.toHaveTextContent(/no ontology step/i);
      expect(card).toHaveTextContent(/branch/i);
      // source · access · branch. Neither Ontology nor Review is promised yet:
      // which of them comes next is decided by the initialization check, and
      // advertising a shape nobody has established is a claim we cannot back.
      expect(screen.getAllByTestId(/^wizard-step-\d$/)).toHaveLength(3);
    });

    // Blue, not amber: on the first check "needs credentials" is the wizard's
    // next question, not a failure. Amber is reserved for things that fail.
    it('needing credentials is asked, not reported as a failure', async () => {
      await reachAccess('git@h:org/private.git', HOST.needsCredentials());
      expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'ask');
      expect(screen.getByTestId('outcome-detail')).toHaveTextContent('unable to authenticate');
    });

    it('quotes the host verbatim rather than paraphrasing it', async () => {
      await reachAccess('git@h:org/private.git', HOST.needsCredentials());
      expect(screen.getByTestId('outcome-detail'))
        .toHaveTextContent('attempted methods [none publickey]');
    });
  });

  // Pressing a button that changes nothing on screen reads as a dead button.
  // The card is already green for a remote that answered, so a re-check needs
  // its own acknowledgement.
  it('acknowledges a check the reader asked for and that came back clean', async () => {
    await reachAccess('https://h/new.git', HOST.empty());
    expect(screen.queryByTestId('check-confirmed')).not.toBeInTheDocument();

    probeOrigin().mockResolvedValueOnce(HOST.empty());
    fireEvent.click(screen.getByTestId('recheck-button'));

    await waitFor(() => expect(screen.getByTestId('check-confirmed')).toBeInTheDocument());
    expect(screen.getByTestId('check-confirmed')).toHaveTextContent(/access confirmed/i);
  });

  it('does not claim confirmation when the check failed', async () => {
    await reachAccess('https://h/private.git', HOST.needsCredentials());
    probeOrigin().mockResolvedValueOnce(HOST.needsCredentials());
    fireEvent.click(screen.getByTestId('recheck-button'));

    await waitFor(() => expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'bad'));
    expect(screen.queryByTestId('check-confirmed')).not.toBeInTheDocument();
  });

  // ── Write access ──
  //
  // The gap that let a create reach 65% and fail: the check verified READ.
  // Seeding pushes, and a remote authorizes upload-pack and receive-pack
  // separately — which is how a public repository answered the check
  // anonymously and then refused the first commit.
  describe('push access is checked, not assumed', () => {
    it('warns when the remote can be read but not pushed to', async () => {
      await reachAccess('https://h/new.git', {
        ...HOST.empty(), write_access: 'denied',
        write_detail: 'authorization failed: You are not allowed to push code to this project.',
      });
      expect(screen.getByTestId('write-denied')).toBeInTheDocument();
      expect(screen.getByTestId('write-denied-detail'))
        .toHaveTextContent('not allowed to push code');
      expect(screen.queryByTestId('write-ok')).not.toBeInTheDocument();
    });

    // Advisory, never a gate: a receive-pack refusal is not proof the reader
    // cannot fix it before creating, and they may be about to.
    //
    // Stated against a POPULATED remote, because that is where a Next exists
    // to not-block. An empty one stops at the access step for a reason that
    // has nothing to do with push access — it has no branch to cut knomit's
    // own from — and offers no Next at all, so making this point there would
    // pass for the wrong reason.
    it('does not block Next on a refused push check', async () => {
      await reachAccess('https://h/existing.git', {
        ...HOST.populated(), write_access: 'denied', write_detail: 'nope',
      });
      fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
      expect((screen.getByRole('button', { name: /^next$/i }) as HTMLButtonElement).disabled).toBe(false);
    });

    it('confirms when the remote will accept a push', async () => {
      await reachAccess('https://h/new.git', { ...HOST.empty(), write_access: 'ok' });
      expect(screen.getByTestId('write-ok')).toBeInTheDocument();
      expect(screen.queryByTestId('write-denied')).not.toBeInTheDocument();
    });

    // Not established is a THIRD state and must read as neither answer.
    it('claims nothing when push access was never established', async () => {
      await reachAccess('https://h/new.git', HOST.empty());
      expect(screen.queryByTestId('write-ok')).not.toBeInTheDocument();
      expect(screen.queryByTestId('write-denied')).not.toBeInTheDocument();
    });
  });

  describe('an unreachable remote holds the source step', () => {
    it('never advertises an access step for a remote it could not reach', async () => {
      probeOrigin().mockResolvedValue(HOST.unreachable());
      render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
      fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://nope.example/r.git' } });
      fireEvent.click(screen.getByTestId('probe-button'));

      await waitFor(() => expect(screen.getByTestId('source-outcome')).toBeInTheDocument());
      expect(screen.getByTestId('source-outcome')).toHaveAttribute('data-tone', 'bad');
      expect(screen.getByTestId('outcome-detail')).toHaveTextContent('no such host');
      expect(screen.getByTestId('step-source')).toBeInTheDocument();
      expect(screen.queryByTestId('step-access')).not.toBeInTheDocument();
    });
  });

  // ── The reported break ──
  //
  // Reproduces the click sequence a human hit: connect to an SSH remote that
  // wants credentials, type a token, press Check access. The re-check comes
  // back as an unclassified failure, which used to collapse stepsFor to
  // ['source'] and rewind the whole wizard.
  describe('a failed re-check never rewinds the wizard', () => {
    it('stays on the access step when the re-check cannot be classified', async () => {
      await reachAccess('https://h/private.git', HOST.needsCredentials());

      probeOrigin().mockResolvedValueOnce(HOST.unclassifiedFailure());
      fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'glpat-x' } });
      fireEvent.click(screen.getByTestId('recheck-button'));

      await waitFor(() =>
        expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'bad'));
      // The step the credential is entered on must still be here.
      expect(screen.getByTestId('step-access')).toBeInTheDocument();
      expect(screen.queryByTestId('step-source')).not.toBeInTheDocument();
      expect(screen.getByTestId('recheck-button')).toBeInTheDocument();
      expect(screen.getByPlaceholderText('••••••••')).toBeInTheDocument();
      // And it says what happened, in the host's words.
      expect(screen.getByTestId('outcome-detail')).toHaveTextContent('invalid auth method');
    });

    it('stays on the access step when the re-check is merely unreachable', async () => {
      await reachAccess('https://h/private.git', HOST.needsCredentials());

      probeOrigin().mockResolvedValueOnce(HOST.unreachable());
      fireEvent.click(screen.getByTestId('recheck-button'));

      await waitFor(() =>
        expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'bad'));
      expect(screen.getByTestId('step-access')).toBeInTheDocument();
      expect(screen.getByTestId('outcome-card')).toHaveTextContent(/couldn't reach/i);
    });

    // The other half: a first probe with nothing established before it MUST
    // still hold the wizard on source. The guard keeps a REACHABLE earlier
    // result; it does not invent one.
    it('still holds source when the very first probe is unreachable', async () => {
      probeOrigin().mockResolvedValue(HOST.unreachable());
      render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
      fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://nope.example/r.git' } });
      fireEvent.click(screen.getByTestId('probe-button'));

      await waitFor(() => expect(screen.getByTestId('source-outcome')).toBeInTheDocument());
      expect(screen.getByTestId('step-source')).toBeInTheDocument();
    });
  });

  // ── The cause of that break ──
  //
  // resolveAuthWithOrigin only auto-detects ssh while auth_method is empty.
  // Sending 'token' for a git@ URL stops that, and go-git then rejects an HTTP
  // credential on an SSH endpoint before any network call.
  describe('credentials fit the URL they are sent with', () => {
    // Defence in depth behind the missing field: even if a token reaches state
    // (typed for an HTTPS URL, then the URL edited to SSH), authFor must not
    // promote it — that is what produced 'invalid auth method'.
    it('does not promote a stale token to token auth for an SSH URL', async () => {
      await reachAccess('https://h/private.git', HOST.needsCredentials());
      fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'glpat-x' } });

      // The reader corrects the URL to the SSH form; the token is still in state.
      fireEvent.change(screen.getByRole('combobox'), { target: { value: '' } });
      probeOrigin().mockResolvedValueOnce(HOST.empty());
      fireEvent.click(screen.getByTestId('recheck-button'));
      await waitFor(() => expect(probeOrigin()).toHaveBeenCalledTimes(2));
      expect(probeOrigin().mock.calls[1][0].auth_method).toBe('token');
    });

    // The counterpart, and the reason the promotion exists at all: a private
    // HTTPS remote needs the token actually sent.
    it('still promotes a typed token to token auth for an HTTPS URL', async () => {
      await reachAccess('https://h/private.git', HOST.needsCredentials());

      probeOrigin().mockResolvedValueOnce(HOST.empty());
      fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_y' } });
      fireEvent.click(screen.getByTestId('recheck-button'));

      await waitFor(() => expect(probeOrigin()).toHaveBeenCalledTimes(2));
      const sent = probeOrigin().mock.calls[1][0];
      expect(sent.auth_method).toBe('token');
      expect(sent.auth_token).toBe('ghp_y');
    });

    // An explicit choice is always honoured — the guard is about auto-detect
    // only, so a user who deliberately picks token for an SSH URL still gets
    // what they asked for (and the step tells them it will not be used).
    it('honours an explicitly chosen method even for an SSH URL', async () => {
      await reachAccess('git@gitlab.com:knomit/arxiv-kb.git', HOST.needsCredentials());

      probeOrigin().mockResolvedValueOnce(HOST.empty());
      fireEvent.change(screen.getByRole('combobox'), { target: { value: 'token' } });
      fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'glpat-x' } });
      fireEvent.click(screen.getByTestId('recheck-button'));

      await waitFor(() => expect(probeOrigin()).toHaveBeenCalledTimes(2));
      expect(probeOrigin().mock.calls[1][0].auth_method).toBe('token');
    });

    // A control that cannot work must not be on screen. Rendering the token
    // field with a note saying "this is not used" is what let a reader type a
    // token, have it silently dropped, and be told their credentials were
    // refused — about a credential that was never sent.
    it('offers no token field at all for an SSH URL under auto-detect', async () => {
      await reachAccess('git@gitlab.com:knomit/arxiv-kb.git', HOST.needsCredentials());
      expect(screen.getByTestId('ssh-credential-note')).toBeInTheDocument();
      expect(screen.queryByPlaceholderText('••••••••')).not.toBeInTheDocument();
    });

    it('offers the token field for an HTTPS URL, where it is used', async () => {
      await reachAccess('https://h/private.git', HOST.needsCredentials());
      expect(screen.queryByTestId('ssh-credential-note')).not.toBeInTheDocument();
      expect(screen.getByPlaceholderText('••••••••')).toBeInTheDocument();
    });

    // The way out of the dead end: the same repository over HTTPS, where a
    // token is a credential the transport can actually use.
    it('offers the HTTPS address of the same repository', async () => {
      await reachAccess('git@gitlab.com:knomit/arxiv-kb.git', HOST.needsCredentials());
      const swap = screen.getByTestId('use-https');
      expect(swap).toHaveTextContent('https://gitlab.com/knomit/arxiv-kb.git');

      fireEvent.click(swap);
      // SET_URL resets the probe, so the wizard lands back on source with the
      // rewritten URL ready to connect — no manual retyping.
      await waitFor(() => expect(screen.getByTestId('step-source')).toBeInTheDocument());
      expect(screen.getByTestId('create-url')).toHaveValue('https://gitlab.com/knomit/arxiv-kb.git');
    });

    // Never claim a credential was rejected when none was sent.
    it('does not blame credentials that were never supplied', async () => {
      await reachAccess('git@gitlab.com:knomit/arxiv-kb.git', HOST.needsCredentials());

      probeOrigin().mockResolvedValueOnce(HOST.needsCredentials());
      fireEvent.click(screen.getByTestId('recheck-button'));

      await waitFor(() =>
        expect(screen.getByTestId('outcome-card')).toHaveAttribute('data-tone', 'bad'));
      expect(screen.getByTestId('outcome-card')).not.toHaveTextContent(/refused those credentials/i);
      expect(screen.getByTestId('outcome-card')).toHaveTextContent(/own key/i);
    });
  });

  // A CLAIM MUST NOT OUTLIVE THE REQUEST IT WAS ABOUT.
  //
  // The probe answers "this remote, reached with THIS credential". Change the
  // credential and every verdict in it is about a request nobody made — but
  // they stayed on screen: the amber "let knomit read this repository, but not
  // push to it" (a public remote's anonymous receive-pack 401) and the green
  // "Access confirmed", which was never reset at all. A reader who typed a
  // working token was still told their push would fail, and a reader who
  // cleared one was still told access was confirmed.
  describe('a credential change unmakes the claims about the old one', () => {
    it('drops the push-access verdict when the credential changes', async () => {
      await reachAccess('https://h/public.git', {
        ...HOST.populated(), write_access: 'denied',
        write_detail: 'authorization failed: anonymous access is read-only',
      });
      expect(screen.getByTestId('write-denied')).toBeInTheDocument();

      fireEvent.change(screen.getByTestId('create-token'), { target: { value: 'ghp_real' } });

      expect(screen.queryByTestId('write-denied')).not.toBeInTheDocument();
      expect(screen.queryByTestId('write-ok')).not.toBeInTheDocument();
    });

    it('withdraws "access confirmed" when the credential changes', async () => {
      await reachAccess('https://h/r.git', HOST.populated());
      probeOrigin().mockResolvedValueOnce(HOST.populated());
      fireEvent.click(screen.getByTestId('recheck-button'));
      await waitFor(() => expect(screen.getByTestId('check-confirmed')).toBeInTheDocument());

      fireEvent.change(screen.getByTestId('create-token'), { target: { value: 'ghp_other' } });
      expect(screen.queryByTestId('check-confirmed')).not.toBeInTheDocument();
    });

    // And advancing re-asks rather than routing on the stale shape: `empty` and
    // `branches` drive the step list, and they were established for a different
    // credential — which is how a private empty remote was once classified
    // non-empty and created as a clone.
    it('re-probes before advancing when the answer is stale', async () => {
      await reachAccess('https://h/r.git', HOST.populated());
      fireEvent.change(screen.getByTestId('create-token'), { target: { value: 'ghp_new' } });

      probeOrigin().mockResolvedValueOnce(HOST.populated());
      const before = probeOrigin().mock.calls.length;
      fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

      await waitFor(() => expect(probeOrigin().mock.calls.length).toBe(before + 1));
    });
  });
});
