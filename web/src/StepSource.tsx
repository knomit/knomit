import type { WizardAction, WizardState } from './wizardState';
import { btn } from './manageStyles';

// StepSource renders the wizard's FIRST screen only — the two peer cards plus
// the URL field. It does not collect a name: local-only's name arrives on its
// own dedicated 'name' step (see CreateRepoWizard.tsx), and a reachable
// remote's name arrives on StepAccess — exactly what stepsFor(state) already
// says for each case. (An earlier version of this file also rendered the name
// field here, to make App.norepos.test.tsx's `create-name` proxy pass on
// first mount without a click; that forced the source step to double as a
// name step. The fix was to point the test's proxy at `step-source` instead —
// it only ever meant "Manage fell back to the create surface" — not to bend
// this component's layout to match the old flat form's DOM shape.)
export function StepSource({ state, dispatch, onProbe, onCancelProbe, probing, probeError }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
  onProbe: () => void;
  onCancelProbe: () => void;
  probing: boolean;
  probeError: string;
}) {
  return (
    <div data-testid="step-source">
      <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap' }}>
        {/* Peer #1, listed first with the fuller description: connecting a
            remote is the path that keeps a repo's full history the moment it
            gains one, so it leads — carried by ordering and detail, not a
            badge. A badge on either peer would make the other read as wrong. */}
        <div style={{ ...card, flex: '1 1 260px' }}>
          <button type="button" style={cardTitleBtn} onClick={() => dispatch({ type: 'CHOOSE_REMOTE' })}>
            Connect a git repository
          </button>
          <p style={cardBody}>
            Point this at a git remote — GitHub, GitLab, Bitbucket, or a bare
            repo reachable over SSH — and knomit keeps the knowledge base in
            sync with it from the first commit.
          </p>
          <label style={label}>Remote URL</label>
          <input data-testid="create-url" style={input} placeholder="https://… · git@host:repo · /path/to/repo"
            value={state.url} onChange={e => dispatch({ type: 'SET_URL', url: e.target.value })} />
          <div style={{ marginTop: 8, display: 'flex', gap: 8 }}>
            <button type="button" data-testid="probe-button" style={btn(!state.url.trim() || probing, 'primary')}
              disabled={!state.url.trim() || probing} onClick={onProbe}>
              {probing ? 'Checking…' : 'Connect'}
            </button>
            {/* Only while a check is in flight, and labelled distinctly from
                the wizard's own Cancel in the footer — this stops the probe,
                it does not leave the wizard. A remote that never answers is
                bounded server-side by the configured network timeout, but that
                budget is minutes long and the user should not have to sit
                through it to fix a typo. */}
            {probing && (
              <button type="button" data-testid="probe-cancel-button" style={btn(false)} onClick={onCancelProbe}>
                Cancel check
              </button>
            )}
          </div>
          {/* Amber is reserved for a probe that actually failed — this is the
              one place StepSource can show a real failure (unreachable host). */}
          {probeError && <div style={warnText}>{probeError}</div>}
          <p style={hint}>
            No repository there yet? Create an EMPTY one first — no README, no
            .gitignore, no license — knomit needs to write the first commit
            itself.{' '}
            {/* target="_blank": the desktop build is a WKWebView, so an
                in-frame navigation here would strand the reader with no way
                back. */}
            <a href="https://knomit.io/docs" target="_blank" rel="noreferrer" style={link}>Learn more</a>.
          </p>
        </div>

        <div style={{ ...card, flex: '1 1 200px' }}>
          <div style={cardTitle}>Keep it on this machine</div>
          <p style={cardBody}>Start locally now, connect a remote whenever you like.</p>
          <button type="button" style={btn(false)} onClick={() => dispatch({ type: 'CHOOSE_LOCAL' })}>
            Keep it on this machine
          </button>
        </div>
      </div>
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const card: React.CSSProperties = { padding: '10px 12px', background: '#111', border: '1px solid #2a2a2a', borderRadius: 6 };
const cardTitle: React.CSSProperties = { fontSize: 14, fontWeight: 600, color: '#eee', marginBottom: 4 };
const cardTitleBtn: React.CSSProperties = { ...cardTitle, background: 'none', border: 'none', padding: 0, cursor: 'pointer', display: 'block', textAlign: 'left' };
const cardBody: React.CSSProperties = { fontSize: 12, color: '#999', lineHeight: 1.5, margin: '4px 0 8px' };
const link: React.CSSProperties = { color: '#6ea8fe' };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 8 };
