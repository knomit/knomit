import { fieldInput, fieldLabel, fieldHint } from './manageStyles';
import { isSSHURL } from './originAuth';
import type { PastedCredential } from './secrets';

// PastedCredentialFields types a credential in for one remote: a token or a
// username and password over http(s), or this server's own SSH key over SSH.
// The inputs are write-only (type="password", never prefilled).
export function PastedCredentialFields({ url, value, onChange, testid = 'pasted' }: {
  url: string;
  value: PastedCredential;
  onChange: (p: PastedCredential) => void;
  testid?: string;
}) {
  if (isSSHURL(url.trim())) {
    return <div style={fieldHint} data-testid={`${testid}-ssh`}>Uses this server&apos;s own SSH key.</div>;
  }
  const set = (patch: Partial<PastedCredential>) => onChange({ ...value, ...patch });
  return (
    <div data-testid={testid} style={{ maxWidth: 420 }}>
      <label style={fieldLabel} htmlFor={`${testid}-method`}>Type</label>
      <select id={`${testid}-method`} data-testid={`${testid}-method`} style={fieldInput} value={value.auth_method}
        onChange={e => set({ auth_method: e.target.value as PastedCredential['auth_method'] })}>
        <option value="token">Token</option>
        <option value="basic">Username + password</option>
      </select>
      {value.auth_method === 'token' && (
        <>
          <label style={fieldLabel} htmlFor={`${testid}-token`}>Token</label>
          <input id={`${testid}-token`} data-testid={`${testid}-token`} type="password" autoComplete="new-password"
            style={fieldInput} value={value.token} onChange={e => set({ token: e.target.value })} />
        </>
      )}
      {value.auth_method === 'basic' && (
        <>
          <label style={fieldLabel} htmlFor={`${testid}-user`}>Username</label>
          <input id={`${testid}-user`} data-testid={`${testid}-user`} autoComplete="off"
            style={fieldInput} value={value.user} onChange={e => set({ user: e.target.value })} />
          <label style={fieldLabel} htmlFor={`${testid}-password`}>Password</label>
          <input id={`${testid}-password`} data-testid={`${testid}-password`} type="password" autoComplete="new-password"
            style={fieldInput} value={value.password} onChange={e => set({ password: e.target.value })} />
        </>
      )}
    </div>
  );
}
