import type { WizardAction, WizardState } from './wizardState';
import { btn, cardLabel } from './manageStyles';

// StepSource renders the wizard's first screen — name it, then say where it
// comes from — and doubles as the confirmation screen once "Keep it on this
// machine" has been chosen (currentStep === 'name'; there is no dedicated
// StepName file, since local-only has nothing left to ask beyond the name
// already collected here). `confirmOnly` selects that second rendering.
//
// The name field is unconditional and always first: a zero-repo install lands
// here with nothing else on screen, and the create surface must show a name
// field the instant it mounts (App.norepos.test.tsx) — it cannot be gated
// behind choosing a source first.
export function StepSource({ state, dispatch, onProbe, probing, probeError, confirmOnly }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
  onProbe: () => void;
  probing: boolean;
  probeError: string;
  confirmOnly: boolean;
}) {
  return (
    <div data-testid="step-source">
      <label style={label}>Name</label>
      {/* autoCapitalize/autoCorrect/spellCheck off: the desktop WKWebView otherwise
          capitalizes/substitutes the typed name (e.g. "test" → "Test"), which fails
          the lowercase-only isValidRepoName check with a confusing 400. See
          CreateRepoForm.tsx:81-83, which this replaces but does not delete. */}
      <input data-testid="create-name" style={input} placeholder="e.g. work (a–z, 0–9, -, _)" value={state.name}
        autoCapitalize="off" autoCorrect="off" spellCheck={false}
        onChange={e => dispatch({ type: 'SET_NAME', name: e.target.value })} />

      {confirmOnly ? (
        <div style={{ ...card, marginTop: 14 }}>
          <div style={cardLabel}>Local-only</div>
          <div style={hint}>Nothing leaves this machine until you connect a remote later.</div>
        </div>
      ) : (
        <div style={{ display: 'flex', gap: 12, marginTop: 14, flexWrap: 'wrap' }}>
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
            <div style={{ marginTop: 8 }}>
              <button type="button" data-testid="probe-button" style={btn(!state.url.trim() || probing, 'primary')}
                disabled={!state.url.trim() || probing} onClick={onProbe}>
                {probing ? 'Checking…' : 'Connect'}
              </button>
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
      )}
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
