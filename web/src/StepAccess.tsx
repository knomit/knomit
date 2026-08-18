import type { WizardAction, WizardState } from './wizardState';
import { isValidRepoName, transportFor, repoNameFromURL, hostOf, httpsFromSSH, probeIsCurrent } from './wizardState';
import { OutcomeCard } from './OutcomeCard';
import { btn } from './manageStyles';

// StepAccess is reached only for a reachable remote (stepsFor never lists it
// otherwise), and it is reached for EVERY reachable remote — including the two
// that succeeded. That is what the outcome card at the top is for: this step
// used to open with a name field and a grey paragraph, so a successful check
// arrived here with no confirmation that anything had worked, and a check that
// needed credentials got a generic sentence while the server's own explanation
// (ProbeResult.Detail) was dropped on the floor.
//
// Order is the same in all four states — card, name, credentials, check —
// because the card now carries the urgency and a conditional field order would
// make the step a different shape each time you saw it.
//
// The credential block is UNCONDITIONAL for every remote flow, not gated on
// probe.auth_required. Hiding it once already broke private HTTPS (the deleted
// CreateRepoForm's `promotes auto-detect + token to token auth` test exists
// because of that), and gating it on auth_required broke the same case from
// the other side: a PUBLIC empty repo over HTTPS probes fine anonymously
// (auth_required:false) but still needs a token to PUSH the seed commit, and
// with the fields hidden the user had no way to supply one. Under auto-detect
// these are optional fields, exactly as they were on the old form.
//
// The re-probe button is the other half. An auth_required probe cannot see
// whether the remote is empty — ProbeOrigin reports {auth_required:true,
// empty:false} because it could not look — so a step list derived from it says
// "clone" for a remote that may well be empty. Re-probing WITH the credentials
// is the only way to turn that unknown into an answer, which is why this step
// can run one.
/** Which failure a probeError describes.
 *
 *  'no-credential' is its own case, not a flavour of 'refused': the check ran
 *  with knomit's own key because nothing was supplied to use instead, so
 *  telling the reader their credentials were rejected would name a cause that
 *  did not occur. */
export type ProbeFailure = '' | 'refused' | 'no-credential' | 'unreachable';

export function StepAccess({ state, dispatch, onProbe, onCancelProbe, probing, probeError, probeFailure, checkedOk = 0 }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
  onProbe: () => void;
  onCancelProbe: () => void;
  probing: boolean;
  probeError: string;
  /** Which failure probeError describes, so the card need not sniff the text. */
  probeFailure?: ProbeFailure;
  /** Increments on each clean check the user asked for; drives the acknowledgement. */
  checkedOk?: number;
}) {
  const probe = state.probe;

  // Every claim below is about the request the probe was MADE with. When the
  // URL or the credential has changed since, the probe still describes a real
  // remote — it is just no longer describing the one this form would now ask
  // about, so nothing here may quote it. The card itself stays (throwing the
  // probe away would rewind the wizard off this step, taking the only place a
  // credential can be typed with it); the verdicts go quiet until a re-check.
  const current = probeIsCurrent(state);
  const authRequired = !!probe?.auth_required;
  const nameTyped = state.name !== '';
  const transport = transportFor(state.url);
  const host = hostOf(state.url);
  // Auto-detect + an SSH URL is the one combination where a token field would
  // be a control that cannot work — see the note it renders instead.
  const sshNoToken = transport === 'ssh' && state.authMethod === '';
  const named = host || 'The remote';
  const httpsAlternative = sshNoToken ? httpsFromSSH(state.url) : '';

  return (
    <div data-testid="step-access">
      {probe && <Outcome state={state} probeError={probeError} probeFailure={probeFailure ?? ''} />}

      <label style={label}>Name</label>
      {/* Same WKWebView guard as StepSource's name field — see CreateRepoForm.tsx:81-83. */}
      <input data-testid="create-name" style={input} placeholder="e.g. work (a–z, 0–9, -, _)" value={state.name}
        autoCapitalize="off" autoCorrect="off" spellCheck={false}
        onChange={e => dispatch({ type: 'SET_NAME', name: e.target.value })} />
      {/* Says the RULE, not just that the value is wrong: a disabled Next with
          no explanation is the same dead end as the backend's 400, moved. */}
      {nameTyped && !isValidRepoName(state.name) && (
        <div data-testid="name-invalid" style={warnText}>Use lowercase letters, digits, - and _ only.</div>
      )}
      {/* Shown only while the field still holds what the URL produced. Compared
          rather than tracked with a flag: PROBE_DONE's prefill IS
          repoNameFromURL(url), so equality answers "is this still the offered
          default?" without a second piece of state that could drift from it.
          A user who types the same name gets the same true statement. */}
      {state.name !== '' && state.name === repoNameFromURL(state.url) && (
        <div data-testid="name-prefilled" style={prefill}>
          taken from the remote — edit if you'd rather call it something else
        </div>
      )}

      <label style={label}>Auth method</label>
      <select style={input} value={state.authMethod} onChange={e => dispatch({ type: 'SET_AUTH_METHOD', method: e.target.value })}>
        <option value="">auto-detect</option>
        <option value="none">none</option>
        <option value="token">token</option>
        <option value="basic">basic</option>
        <option value="ssh">ssh</option>
      </select>
      {authRequired ? (
        <div data-testid="auth-required-hint" style={hint}>
          knomit cannot see whether this remote already has any history until it
          can get in. What you enter here decides whether it joins the remote or
          writes its first commit.
        </div>
      ) : (
        <div style={hint}>
          Optional — the check above already succeeded. A private remote over
          HTTPS still needs a token here to WRITE, even when reading it worked.
        </div>
      )}
      {/* No token field for an SSH URL under auto-detect.

          An earlier version of this step DID render one, with a note saying it
          would not be used. That is the worst available design: a token typed
          there was silently dropped by authFor, the probe fell back to knomit's
          own key, and the failure came back worded as "those credentials did
          not give access" — about a credential that was never sent. A control
          that cannot work must not be on screen; the way out goes here instead.

          An explicitly chosen method is still honoured below — this only
          governs auto-detect, where knomit is the one choosing. */}
      {sshNoToken ? (
        <div data-testid="ssh-credential-note" style={schemeNote}>
          This URL connects over SSH, so knomit authenticates with an <b style={{ color: '#a8c4dc' }}>SSH
          key</b> — a token or password cannot be used with it. knomit offers its own key, so
          {host ? ` ${host}` : ' the host'} has to be told to accept that key.
          {httpsAlternative && (
            <>
              {' '}Or use this repository's HTTPS address, where a token does work:
              <div style={{ marginTop: 8 }}>
                <button type="button" data-testid="use-https" style={btn(false)}
                  onClick={() => dispatch({ type: 'SET_URL', url: httpsAlternative })}>
                  Use {httpsAlternative}
                </button>
              </div>
            </>
          )}
        </div>
      ) : null}
      {state.authMethod === 'basic' && (
        <>
          <label style={label}>Username</label>
          <input style={input} placeholder="username" value={state.authUser}
            onChange={e => dispatch({ type: 'SET_AUTH_USER', user: e.target.value })} />
          {state.authUser.trim() === '' && <div style={hint}>Basic auth requires a username.</div>}
        </>
      )}
      {!sshNoToken && (state.authMethod === '' || state.authMethod === 'token' || state.authMethod === 'basic') && (
        <>
          <label style={label}>{state.authMethod === 'basic' ? 'Password' : 'Token / password'}</label>
          <input data-testid="create-token" style={input} type="password" placeholder="••••••••" value={state.authToken}
            onChange={e => dispatch({ type: 'SET_TOKEN', token: e.target.value })} />
        </>
      )}

      <div style={{ marginTop: 10, display: 'flex', gap: 8 }}>
        <button type="button" data-testid="recheck-button" style={btn(probing)} disabled={probing} onClick={onProbe}>
          {/* One label in every state. It named the wizard's history rather
              than the action ("Check again" once a check had already passed),
              which tells the reader nothing about what pressing it does. */}
          {probing ? 'Checking…' : 'Check access'}
        </button>
        {/* Same abortable check as StepSource's, for the same reason: this
            step's Next runs one too, and neither may freeze the wizard. */}
        {probing && (
          <button type="button" data-testid="probe-cancel-button" style={btn(false)} onClick={onCancelProbe}>
            Cancel check
          </button>
        )}
      </div>
      {/* Advisory, not a gate. knomit writes every fact as a commit, so a
          remote it can read but not push to is a repository that fails at
          create (seed) or stops syncing later (clone) — but a receive-pack
          refusal is not proof the reader cannot fix it, and they may be about
          to add the very key or token that does. Amber, because this one WILL
          fail if left alone. */}
      {current && probe?.write_access === 'denied' && !probing && (
        <div data-testid="write-denied" style={writeWarn}>
          <div style={{ fontWeight: 600, marginBottom: 4 }}>
            {named} let knomit read this repository, but not push to it.
          </div>
          <div>
            knomit writes every fact as a commit, so it needs push access
            {state.probe?.empty ? ' — starting with the first commit it writes here' : ''}.
            {' '}Supply a credential with write access, or grant it to the one you gave.
          </div>
          {probe.write_detail && (
            <pre data-testid="write-denied-detail" style={writeWarnPre}>{probe.write_detail}</pre>
          )}
        </div>
      )}
      {current && probe?.write_access === 'ok' && !probing && !probeError && (
        <div data-testid="write-ok" style={confirmed}>
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="#7c9" strokeWidth="3"
            strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <polyline points="20 6 9 17 4 12" />
          </svg>
          <span>knomit can push to this remote.</span>
        </div>
      )}
      {current && checkedOk > 0 && !probing && !probeError && (
        <div data-testid="check-confirmed" style={confirmed}>
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="#7c9" strokeWidth="3"
            strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <polyline points="20 6 9 17 4 12" />
          </svg>
          <span>Access confirmed{host ? ` for ${host}` : ''}.</span>
        </div>
      )}
      {authRequired && (
        <div data-testid="next-gate-hint" style={hint}>
          Next unlocks once a check gets in{host ? ` to ${host}` : ''}.
        </div>
      )}
    </div>
  );
}

// Outcome turns a ProbeResult into the card's words. Every sentence below is
// built from a probe FIELD or from what this codebase itself does with the
// URL — never from an inference about why a host answered the way it did.
function Outcome({ state, probeError, probeFailure }: {
  state: WizardState; probeError: string; probeFailure: ProbeFailure;
}) {
  const probe = state.probe;
  if (!probe) return null;
  const host = hostOf(state.url);
  const named = host || 'the remote';
  const transport = transportFor(state.url);
  const via =
    transport === 'ssh' ? 'That URL connects over SSH, so knomit offered its own key.' :
    transport === 'http' ? 'knomit tried it over HTTPS.' :
    'knomit tried it as a local path.';

  // probeError on THIS step means the re-probe the user asked for came back
  // refusing what they supplied — a failure, and the one place amber is earned
  // here. CreateRepoWizard sets it only for that case (and for an unreachable
  // result, which cannot land on this step at all).
  // A failed check is reported without un-establishing a remote that was
  // already reached — PROBE_DONE keeps the successful probe, so the step list
  // (and this step) survive. The two failures read differently: one is the host
  // rejecting a credential, the other is never having got there.
  if (probeError) {
    const headline =
      probeFailure === 'unreachable' ? `Couldn't reach ${named}` :
      probeFailure === 'no-credential' ? `${named} still would not let knomit in` :
      `${named} refused those credentials`;
    const body =
      probeFailure === 'unreachable'
        ? 'The remote answered an earlier check, so this may be temporary. Nothing has been changed.'
        : probeFailure === 'no-credential'
          ? 'The check ran with knomit\u2019s own key, because nothing was supplied to use instead.'
          : 'knomit tried again with what you supplied.';
    return (
      <OutcomeCard
        tone="bad"
        headline={headline}
        url={state.url}
        body={body}
        detail={probeError}
        detailLabel={probeFailure === 'unreachable' ? 'the check reported' : `${named} said`}
      />
    );
  }

  if (probe.auth_required) {
    return (
      <OutcomeCard
        tone="ask"
        headline={`${named} wants credentials`}
        url={state.url}
        body={`${via} Until it can get in, it cannot tell whether this repository has any history.`}
        detail={probe.detail}
        detailLabel={`${named} said`}
      />
    );
  }

  // A remote with NO BRANCHES is where the wizard stops, so this card has to
  // say so and say what fixes it. It is the one reachable outcome that is not
  // a step on the way to a create.
  //
  // It used to read as the opposite — "knomit will write the first commit
  // itself, on main. You'll choose the ontology next" — which described the
  // deleted "seed" mode. That mode pushed the remote's DEFAULT branch, which
  // every host protects on a new project, so the create it promised failed
  // with "pre-receive hook declined" for anyone below Maintainer. The sentence
  // outlived the mode and became a promise the step list cannot keep: the rail
  // shows two steps, there is no ontology step after this one, and nothing
  // writes a first commit.
  //
  // Tone 'ask', not 'bad': nothing failed here. The check reached the host and
  // got a clear answer; knomit needs one thing from the user before it can go
  // on, which is exactly what 'ask' is for. Amber is reserved for failures
  // (kb/conventions/ui/copy/warning-styling-reserved-for-failures).
  //
  // The requirement is stated as "a branch exists", not as "add a README":
  // that checkbox is one host's way of producing a first commit, and writing
  // the guidance around it would turn an accident of a UI into an apparent
  // rule of knomit. Both ways are offered, neither is the rule.
  if (probe.empty) {
    return (
      <OutcomeCard
        tone="ask"
        headline={`Reached ${named} — the repository has no branches`}
        url={state.url}
        body={
          // The instruction FIRST. An empty state is an invitation to act, and
          // the reason it is empty is worth nothing to a reader who cannot find
          // what to do about it in the paragraph explaining why.
          <>
            <div>
              Give the remote a <span style={mono}>main</span> branch, then check
              access again. One commit is enough, and it does not matter what is
              in it — ticking “add a README” when you create the project is the
              quickest way, and{' '}
              <span style={mono}>git commit --allow-empty</span> does just as well.
            </div>
            <div style={{ marginTop: 8 }}>
              knomit only ever writes its own branch, cut from one that already
              exists — so it needs a branch here to cut from, and it will not
              touch the one you make.
            </div>
          </>
        }
      />
    );
  }

  const count = probe.branches.length;
  return (
    <OutcomeCard
      tone="good"
      headline={`Reached ${named} — ${count} ${count === 1 ? 'branch' : 'branches'}`}
      url={state.url}
      body="knomit will clone this repository and adopt the ontology already in it — that choice belongs to the remote, so there is no ontology step for this case."
    >
      {count > 0 && (
        <div data-testid="probe-branches" style={{ display: 'flex', flexWrap: 'wrap', gap: 5 }}>
          {probe.branches.map(b => {
            const tracked = b === (state.branch || probe.upstream_branch);
            return (
              <span key={b} style={chip(tracked)}>{b}{tracked ? ' · tracked' : ''}</span>
            );
          })}
        </div>
      )}
    </OutcomeCard>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 14, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5, maxWidth: '68ch' };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 6 };
const prefill: React.CSSProperties = { fontSize: 11, color: '#7c9', marginTop: 5 };
const writeWarn: React.CSSProperties = {
  marginTop: 12, padding: '10px 12px', borderRadius: 6,
  background: '#262013', border: '1px solid #4a3f22',
  fontSize: 12, lineHeight: 1.55, color: '#c9ad78', maxWidth: '74ch',
};
const writeWarnPre: React.CSSProperties = {
  margin: '8px 0 0', padding: '8px 10px', borderRadius: 4,
  background: '#0d0b07', border: '1px solid #33291a', color: '#d9b978',
  fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1.5,
  whiteSpace: 'pre-wrap', wordBreak: 'break-word', maxHeight: 120, overflow: 'auto',
};
const confirmed: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 6,
  fontSize: 12, color: '#7c9', marginTop: 8,
};
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const mono: React.CSSProperties = { fontFamily: 'var(--k-font-mono)' };
const schemeNote: React.CSSProperties = {
  marginTop: 10, padding: '9px 11px', borderRadius: 5,
  background: '#0f1319', border: '1px solid #223244',
  fontSize: 11.5, lineHeight: 1.55, color: '#8ba3ba', maxWidth: '68ch',
};
const chip = (tracked: boolean): React.CSSProperties => ({
  fontFamily: 'var(--k-font-mono)', fontSize: 10.5, padding: '1px 7px', borderRadius: 3,
  background: tracked ? '#14251a' : '#101c2a',
  border: '1px solid ' + (tracked ? '#2a4a2a' : '#24405e'),
  color: tracked ? '#7c9' : '#8af',
});
