import { useState } from 'react';
import { api, type SecretType, type SecretValueFields, type SecretView } from './api';
import { btn, fieldInput, fieldLabel, fieldHint, wizardErr } from './manageStyles';
import { SECRET_TYPE_LABEL, expiryFromDate, parseHosts } from './secrets';

// SecretForm creates a stored secret, or replaces one's value. It is the
// Secrets tab's Add and Replace value, and the inline "Create a new secret"
// of the repo settings, wizard, fleet and restore pickers.
//
// Values are write-only: the inputs are never prefilled, never echoed, and
// cleared once the server has the value (whatever the outcome, a failed
// submit keeps them so the reader can fix a typo).

const ALL_TYPES: SecretType[] = ['token', 'basic', 'ssh_key'];

export function SecretForm({ mode, secret, types = ALL_TYPES, defaultHosts = [], onDone, onCancel }: {
  mode: 'create' | 'replace';
  /** replace: the secret whose value is replaced (its type decides the fields). */
  secret?: Pick<SecretView, 'id' | 'name' | 'type'>;
  /** create: the types offered (a picker offers only those that fit its remote). */
  types?: SecretType[];
  /** create: the allowed hosts to start from (a picker's remote host). */
  defaultHosts?: string[];
  onDone: (view: SecretView) => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState('');
  const [type, setType] = useState<SecretType>(secret?.type ?? types[0] ?? 'token');
  const [hosts, setHosts] = useState(defaultHosts.join(', '));
  const [expiry, setExpiry] = useState('');
  const [value, setValue] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [key, setKey] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const t = mode === 'replace' && secret ? secret.type : type;
  const fields = (): SecretValueFields => {
    switch (t) {
      case 'token': return { value };
      case 'basic': return { username, password };
      default: return passphrase ? { private_key: key, passphrase } : { private_key: key };
    }
  };
  const valueReady = t === 'token' ? value !== '' : t === 'basic' ? username !== '' && password !== '' : key.trim() !== '';
  const ready = valueReady && (mode === 'replace' || (name.trim() !== '' && parseHosts(hosts).length > 0));

  const clearValues = () => { setValue(''); setUsername(''); setPassword(''); setKey(''); setPassphrase(''); };

  const submit = async () => {
    setBusy(true);
    setErr('');
    try {
      const view = mode === 'replace' && secret
        ? await api.patchSecret(secret.id, fields())
        : await api.createSecret({
          name: name.trim(), type: t, hosts: parseHosts(hosts),
          ...(expiry ? { expires_at: expiryFromDate(expiry) } : {}),
          ...fields(),
        });
      clearValues();
      onDone(view);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div data-testid={`secret-form-${mode}`} style={{ maxWidth: 520 }}>
      {mode === 'create' && (
        <>
          <label style={fieldLabel} htmlFor="secret-name">Name</label>
          <input id="secret-name" data-testid="secret-name" style={fieldInput} value={name}
            onChange={e => setName(e.target.value)} placeholder="e.g. github-pat" autoComplete="off" />
          {types.length > 1 && (
            <>
              <label style={fieldLabel} htmlFor="secret-type">Type</label>
              <select id="secret-type" data-testid="secret-type" style={fieldInput} value={type}
                onChange={e => setType(e.target.value as SecretType)}>
                {types.map(x => <option key={x} value={x}>{SECRET_TYPE_LABEL[x]}</option>)}
              </select>
            </>
          )}
          <label style={fieldLabel} htmlFor="secret-hosts">Allowed hosts</label>
          <input id="secret-hosts" data-testid="secret-hosts" style={fieldInput} value={hosts}
            onChange={e => setHosts(e.target.value)} placeholder="github.com" autoComplete="off" />
          <div style={fieldHint}>The secret is only sent to these hosts. Separate several with commas.</div>
          <label style={fieldLabel} htmlFor="secret-expiry">Expires (optional)</label>
          <input id="secret-expiry" data-testid="secret-expiry" type="date" style={fieldInput} value={expiry}
            onChange={e => setExpiry(e.target.value)} />
        </>
      )}
      {mode === 'replace' && secret && (
        <div style={fieldHint}>New value for {secret.name} ({SECRET_TYPE_LABEL[secret.type]}). Repos using it pick it up on their next sync.</div>
      )}
      {t === 'token' && (
        <>
          <label style={fieldLabel} htmlFor="secret-value">Token</label>
          <input id="secret-value" data-testid="secret-value" type="password" style={fieldInput} value={value}
            onChange={e => setValue(e.target.value)} autoComplete="new-password" />
        </>
      )}
      {t === 'basic' && (
        <>
          <label style={fieldLabel} htmlFor="secret-username">Username</label>
          <input id="secret-username" data-testid="secret-username" style={fieldInput} value={username}
            onChange={e => setUsername(e.target.value)} autoComplete="off" />
          <label style={fieldLabel} htmlFor="secret-password">Password</label>
          <input id="secret-password" data-testid="secret-password" type="password" style={fieldInput} value={password}
            onChange={e => setPassword(e.target.value)} autoComplete="new-password" />
        </>
      )}
      {t === 'ssh_key' && (
        <>
          <label style={fieldLabel} htmlFor="secret-key">Private key</label>
          <textarea id="secret-key" data-testid="secret-key" style={{ ...fieldInput, fontFamily: 'var(--k-font-mono)', minHeight: 90 }}
            value={key} onChange={e => setKey(e.target.value)} spellCheck={false} autoComplete="off"
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />
          <label style={fieldLabel} htmlFor="secret-passphrase">Passphrase (optional)</label>
          <input id="secret-passphrase" data-testid="secret-passphrase" type="password" style={fieldInput} value={passphrase}
            onChange={e => setPassphrase(e.target.value)} autoComplete="new-password" />
        </>
      )}
      <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
        <button type="button" data-testid="secret-form-submit" style={btn(busy || !ready, 'primary')}
          disabled={busy || !ready} onClick={submit}>
          {mode === 'create' ? 'Save secret' : 'Replace value'}
        </button>
        <button type="button" data-testid="secret-form-cancel" style={btn(busy)} disabled={busy}
          onClick={() => { clearValues(); onCancel(); }}>Cancel</button>
      </div>
      {err && <div role="alert" data-testid="secret-form-error" style={wizardErr}>{err}</div>}
    </div>
  );
}
