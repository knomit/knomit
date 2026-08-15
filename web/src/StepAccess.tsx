import type { WizardAction, WizardState } from './wizardState';

// StepAccess is reached only for a reachable remote (stepsFor never lists it
// otherwise). Remote flows have no dedicated "name" step — the name field
// lives here instead, alongside credentials when the probe asked for them.
export function StepAccess({ state, dispatch }: { state: WizardState; dispatch: (a: WizardAction) => void }) {
  const authRequired = !!state.probe?.auth_required;
  return (
    <div data-testid="step-access">
      <label style={label}>Name</label>
      {/* Same WKWebView guard as StepSource's name field — see CreateRepoForm.tsx:81-83. */}
      <input data-testid="create-name" style={input} placeholder="e.g. work (a–z, 0–9, -, _)" value={state.name}
        autoCapitalize="off" autoCorrect="off" spellCheck={false}
        onChange={e => dispatch({ type: 'SET_NAME', name: e.target.value })} />

      {authRequired && (
        <>
          <div style={hint}>This remote asked for credentials.</div>
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
        </>
      )}
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 4 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
