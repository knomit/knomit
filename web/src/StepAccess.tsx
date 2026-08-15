import type { WizardAction, WizardState } from './wizardState';
import { isValidRepoName } from './wizardState';
import { btn } from './manageStyles';

// StepAccess is reached only for a reachable remote (stepsFor never lists it
// otherwise). Remote flows have no dedicated "name" step — the name field
// lives here instead, alongside credentials.
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
export function StepAccess({ state, dispatch, onProbe, onCancelProbe, probing, probeError }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
  onProbe: () => void;
  onCancelProbe: () => void;
  probing: boolean;
  probeError: string;
}) {
  const authRequired = !!state.probe?.auth_required;
  const nameTyped = state.name !== '';
  return (
    <div data-testid="step-access">
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

      {authRequired ? (
        <div data-testid="auth-required-hint" style={hint}>
          This remote asked for credentials, so knomit could not see whether it
          already has any history. Enter access details and check them — that
          answer decides whether knomit joins this remote or writes its first
          commit.
        </div>
      ) : (
        <div style={hint}>
          Optional. Leave blank for a public remote or an SSH key already on
          this machine; a private remote over HTTPS needs a token here even
          when the check above succeeded, because writing to it does.
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
      {state.authMethod === 'basic' && (
        <>
          <label style={label}>Username</label>
          <input style={input} placeholder="username" value={state.authUser}
            onChange={e => dispatch({ type: 'SET_AUTH_USER', user: e.target.value })} />
          {state.authUser.trim() === '' && <div style={hint}>Basic auth requires a username.</div>}
        </>
      )}
      {(state.authMethod === '' || state.authMethod === 'token' || state.authMethod === 'basic') && (
        <>
          <label style={label}>{state.authMethod === 'basic' ? 'Password' : 'Token / password'}</label>
          <input style={input} type="password" placeholder="••••••••" value={state.authToken}
            onChange={e => dispatch({ type: 'SET_TOKEN', token: e.target.value })} />
        </>
      )}

      <div style={{ marginTop: 10, display: 'flex', gap: 8 }}>
        <button type="button" data-testid="recheck-button" style={btn(probing)} disabled={probing} onClick={onProbe}>
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
      {/* Amber, because by the time it renders here something FAILED: this is
          a re-check the user asked for with credentials they supplied, so a
          refusal is a refusal — unlike the first, anonymous probe, where
          "needs credentials" is just the next question. */}
      {probeError && <div data-testid="access-error" style={warnText}>{probeError}</div>}
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5 };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 6 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
