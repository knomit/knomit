import type { WizardAction, WizardState } from './wizardState';
import { isValidRepoName, hostOf } from './wizardState';
import { OutcomeCard } from './OutcomeCard';
import { btn } from './manageStyles';

// StepSource asks ONE question — where this repository's history lives — with a
// segmented control, and then discloses only the fields that answer needs.
//
// It used to render the two options as side-by-side peer cards. The intent was
// right (neither may carry a badge: a badge on one makes the other read as
// wrong) but the mechanism was not. Columns imply peers, and these two are not
// shaped alike: remote carries a label, an input, a button and a caveat about
// empty remotes, while local carries one sentence. Side by side, that asymmetry
// reads as importance — so the layout made exactly the ranking claim the
// no-badge rule existed to prevent, and spent the widest row on the screen on a
// binary that one control settles.
//
// As a control instead of a layout, the choice also gets to carry a HUE without
// re-opening that question, because the hue encodes state rather than rank:
// remote is blue (#8af already means branch/remote everywhere else in this UI),
// local is neutral, and the honest difference between them is that one has a
// remote to talk about and the other does not. Amber stays reserved for a probe
// that actually failed — see kb/conventions/ui/copy/warning-styling-reserved-
// for-failures.
//
// The local pane owns the name field, which is why stepsFor's local list no
// longer has a 'name' step. The remote path still collects its name on
// StepAccess, alongside credentials.
export function StepSource({ state, dispatch, onProbe, onCancelProbe, probing, probeError }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
  onProbe: () => void;
  onCancelProbe: () => void;
  probing: boolean;
  probeError: string;
}) {
  const local = state.choice === 'local';
  return (
    <div data-testid="step-source">
      <div style={segGroup} role="group" aria-label="Repository source">
        <button type="button" data-testid="choose-remote" style={segment(!local, 'remote')}
          aria-pressed={!local} onClick={() => dispatch({ type: 'CHOOSE_REMOTE' })}>
          <span style={segDot(!local, 'remote')} />
          <span>
            Connect a git repository
            <span style={segSub(!local, 'remote')}>Synced from the first commit</span>
          </span>
        </button>
        <button type="button" data-testid="choose-local" style={segment(local, 'local')}
          aria-pressed={local} onClick={() => dispatch({ type: 'CHOOSE_LOCAL' })}>
          <span style={segDot(local, 'local')} />
          <span>
            Keep it on this machine
            <span style={segSub(local, 'local')}>Connect a remote later</span>
          </span>
        </button>
      </div>

      <div style={disclosure}>
        {local ? <LocalPane state={state} dispatch={dispatch} /> : (
          <RemotePane state={state} dispatch={dispatch} onProbe={onProbe}
            onCancelProbe={onCancelProbe} probing={probing} probeError={probeError} />
        )}
      </div>
    </div>
  );
}

function RemotePane({ state, dispatch, onProbe, onCancelProbe, probing, probeError }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
  onProbe: () => void;
  onCancelProbe: () => void;
  probing: boolean;
  probeError: string;
}) {
  return (
    <div data-testid="source-pane-remote">
      <label style={label}>Remote URL</label>
      <input data-testid="create-url" style={input} placeholder="https://… · git@host:repo · /path/to/repo"
        value={state.url} onChange={e => dispatch({ type: 'SET_URL', url: e.target.value })} />
      <div style={{ marginTop: 10, display: 'flex', gap: 8 }}>
        <button type="button" data-testid="probe-button" style={btn(!state.url.trim() || probing, 'primary')}
          disabled={!state.url.trim() || probing} onClick={onProbe}>
          {probing ? 'Checking…' : 'Connect'}
        </button>
        {/* Only while a check is in flight, and labelled distinctly from the
            wizard's own Cancel in the footer — this stops the probe, it does
            not leave the wizard. A remote that never answers is bounded
            server-side by the configured network timeout, but that budget is
            minutes long and the user should not have to sit through it to fix
            a typo. */}
        {probing && (
          <button type="button" data-testid="probe-cancel-button" style={btn(false)} onClick={onCancelProbe}>
            Cancel check
          </button>
        )}
      </div>
      <p style={hint}>
        GitHub, GitLab, Bitbucket, or any bare repo reachable over SSH.
      </p>
      {/* The one failure this step can show. stepsFor collapses an unreachable
          probe to ['source'], so this remote never gets an access step and this
          card is the only place its outcome can be reported — which is why it
          is the same component the access step uses rather than a bare amber
          line. The server's words go in `detail` untouched; nothing here
          guesses why the host did not answer. */}
      {probeError && (
        <div style={{ marginTop: 12 }}>
          <OutcomeCard
            testid="source-outcome"
            tone="bad"
            headline={hostOf(state.url) ? `Couldn't reach ${hostOf(state.url)}` : 'Could not reach that remote'}
            detail={probeError}
          />
        </div>
      )}
      {/* This used to say the opposite — "the remote has to be EMPTY, no
          README, no .gitignore, no license" — and it was the instruction that
          broke the feature. knomit obeyed it by pushing the first commit to
          the remote's default branch, which hosts protect on new projects, so
          the create failed outright for anyone below Maintainer. Working
          around it by adding a README then routed the wizard to a mode that
          silently replaced the chosen ontology with the default one.

          The requirement is written as `main` EXISTS rather than as "add a
          README": that checkbox is one host's way of producing a first commit,
          and building the instruction around it would turn an accident of
          someone's UI into an apparent rule of knomit. */}
      <div style={caveat}>
        The remote needs a <b style={{ color: '#8b9199' }}>main</b> branch — one commit
        is enough, and ticking "add a README" is the quickest way to get one.
        knomit works on its own branch and never writes to <b style={{ color: '#8b9199' }}>main</b>,
        so you don't need push access to it.{' '}
        {/* target="_blank": the desktop build is a WKWebView, so an in-frame
            navigation here would strand the reader with no way back. */}
        <a href="https://knomit.io/docs" target="_blank" rel="noreferrer" style={link}>Learn more</a>.
      </div>
    </div>
  );
}

function LocalPane({ state, dispatch }: { state: WizardState; dispatch: (a: WizardAction) => void }) {
  const nameTyped = state.name !== '';
  return (
    <div data-testid="source-pane-local">
      <label style={label}>Name</label>
      {/* Same WKWebView guard as StepAccess's name field — see CreateRepoForm.tsx:81-83. */}
      <input data-testid="create-name" style={input} placeholder="e.g. work (a–z, 0–9, -, _)" value={state.name}
        autoCapitalize="off" autoCorrect="off" spellCheck={false}
        onChange={e => dispatch({ type: 'SET_NAME', name: e.target.value })} />
      {/* Says the RULE, not just that the value is wrong: a disabled Next with
          no explanation is the same dead end as the backend's 400, moved. */}
      {nameTyped && !isValidRepoName(state.name) && (
        <div data-testid="name-invalid" style={warnText}>Use lowercase letters, digits, - and _ only.</div>
      )}
      <p style={hint}>Nothing leaves this machine until you connect a remote later.</p>
    </div>
  );
}

// ── styles ──
//
// REMOTE_ACCENT is #8af, the hue this UI already spends on branches and remote
// refs (CreateLensForm's branch label, RemoteStatus). LOCAL_ACCENT is a plain
// slate: local-only has no remote to name, and inventing a hue for it would be
// decoration. Neither is amber, green or purple — those mean failure, write
// target and lens respectively, and none of the three is what this asks.
const REMOTE_ACCENT = '#8af';
const LOCAL_ACCENT = '#8b9199';

const segGroup: React.CSSProperties = { display: 'flex', gap: 6, flexWrap: 'wrap' };

const segment = (on: boolean, mode: 'remote' | 'local'): React.CSSProperties => ({
  flex: '1 1 220px', display: 'flex', alignItems: 'flex-start', gap: 9,
  padding: '10px 12px', borderRadius: 6, cursor: 'pointer', textAlign: 'left',
  fontSize: 13, fontFamily: 'inherit',
  background: on ? (mode === 'remote' ? '#10161f' : '#17181a') : '#0f0f0f',
  border: '1px solid ' + (on ? (mode === 'remote' ? '#24405e' : '#3a3d42') : '#242424'),
  color: on ? '#eee' : '#999',
});

const segDot = (on: boolean, mode: 'remote' | 'local'): React.CSSProperties => {
  const accent = mode === 'remote' ? REMOTE_ACCENT : LOCAL_ACCENT;
  return {
    width: 9, height: 9, borderRadius: '50%', flexShrink: 0, marginTop: 5,
    background: on ? accent : 'transparent',
    border: '1.5px solid ' + (on ? accent : '#4a4a4a'),
  };
};

const segSub = (on: boolean, mode: 'remote' | 'local'): React.CSSProperties => ({
  display: 'block', fontSize: 11.5, marginTop: 1,
  color: on ? (mode === 'remote' ? '#6a89ad' : '#7d838b') : '#555',
});

// A rule between the question and its answer: the fields below belong to the
// segment above, and without it the pane reads as a second, unrelated section.
const disclosure: React.CSSProperties = {
  borderTop: '1px solid #242424', marginTop: 14, paddingTop: 2,
};

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const caveat: React.CSSProperties = { marginTop: 10, padding: '8px 10px', borderRadius: 5, background: '#131313', border: '1px solid #242424', fontSize: 11.5, lineHeight: 1.55, color: '#666' };
const link: React.CSSProperties = { color: '#6ea8fe' };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 8 };
