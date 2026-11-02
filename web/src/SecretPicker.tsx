import { useCallback, useEffect, useState } from 'react';
import { api, type SecretView } from './api';
import { fieldInput, fieldHint, confirmBox, wizardErr } from './manageStyles';
import { CredentialChip } from './CredentialChip';
import { SecretForm } from './SecretForm';
import { remoteHost, secretFits, typesForURL } from './secrets';

// SecretPicker chooses the stored secret a remote uses: only the secrets that
// fit it are offered (a type that fits the URL's scheme, and a hosts list
// that allows its host), plus, with allowCreate, "Create a new secret…",
// which opens SecretForm with the remote's host already allowed and selects
// the secret it creates. The server checks the fit again when it links.

const NEW = '\u0000new';

export function SecretPicker({ url, value, onChange, allowCreate = false, testid = 'secret-picker' }: {
  /** The remote the secret is for. */
  url: string;
  /** The chosen secret's id, '' for none. */
  value: string;
  onChange: (id: string, secret: SecretView | null) => void;
  allowCreate?: boolean;
  testid?: string;
}) {
  const [all, setAll] = useState<SecretView[] | null>(null);
  const [err, setErr] = useState('');
  const [creating, setCreating] = useState(false);

  const load = useCallback(() => api.listSecrets()
    .then(l => { setErr(''); setAll(l); return l; })
    .catch(e => { setErr(e instanceof Error ? e.message : String(e)); return null; }), []);
  useEffect(() => { load(); }, [load]);

  const host = remoteHost(url);
  const fitting = (all ?? []).filter(s => secretFits(s, url));
  const chosen = fitting.find(s => s.id === value) ?? null;
  const types = typesForURL(url);

  return (
    <div data-testid={testid}>
      {err && <div role="alert" data-testid={`${testid}-error`} style={wizardErr}>{err}</div>}
      {all === null && !err && <div style={fieldHint}>Loading stored secrets…</div>}
      {all !== null && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <select data-testid={`${testid}-select`} aria-label="Stored secret" style={{ ...fieldInput, maxWidth: 360 }}
            value={creating ? NEW : value}
            onChange={e => {
              if (e.target.value === NEW) { setCreating(true); return; }
              setCreating(false);
              onChange(e.target.value, fitting.find(s => s.id === e.target.value) ?? null);
            }}>
            <option value="">{fitting.length > 0 ? 'Choose a stored secret…' : 'No stored secret fits this remote'}</option>
            {fitting.map(s => (
              <option key={s.id} value={s.id}>
                {s.name}{s.status === 'expired' ? ' (expired)' : s.status === 'expiring' ? ' (expiring)' : ''}
              </option>
            ))}
            {allowCreate && types.length > 0 && host && <option value={NEW}>Create a new secret…</option>}
          </select>
          {chosen && <CredentialChip status={chosen.status} name={chosen.name} />}
        </div>
      )}
      {all !== null && fitting.length === 0 && !creating && (
        <div style={fieldHint} data-testid={`${testid}-none`}>
          {host ? `Secrets are listed here when they allow ${host} and fit this kind of URL.` : 'Enter a remote URL first.'}
          {allowCreate && host ? ' Create one here, or in Access → Secrets.' : ' Add one in Access → Secrets.'}
        </div>
      )}
      {creating && (
        <div style={confirmBox}>
          <SecretForm mode="create" types={types} defaultHosts={host ? [host] : []}
            onCancel={() => setCreating(false)}
            onDone={async v => {
              setCreating(false);
              await load();
              onChange(v.id, v);
            }} />
        </div>
      )}
    </div>
  );
}
