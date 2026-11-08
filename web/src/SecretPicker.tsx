import { useCallback, useContext, useEffect, useState } from 'react';
import { api, type SecretView } from './api';
import { fieldInput, fieldHint, linkBtn, wizardErr } from './manageStyles';
import { CredentialChip } from './CredentialChip';
import { AccessSecretsLink } from './accessLink';
import { remoteHost, secretFits } from './secrets';

// SecretPicker chooses the stored secret a remote uses. It lists ONLY the
// secrets that fit the remote (a type that fits the URL's scheme, and a hosts
// list that allows its host); the server checks the fit again when it links.
//
// - Some fit: the select holds just those, and the first is chosen as soon as
//   the list arrives (or when the chosen one stops fitting). Nothing links on
//   that: the caller still asks the reader to test and save.
// - None fit: there is no select, only a line pointing at Access → Secrets,
//   and the value is cleared, so a caller that gates on it stays disabled.
//
// It never creates a secret: that happens only in Access → Secrets.

export function SecretPicker({ url, value, onChange, testid = 'secret-picker' }: {
  /** The remote the secret is for. */
  url: string;
  /** The chosen secret's id, '' for none. */
  value: string;
  onChange: (id: string, secret: SecretView | null) => void;
  testid?: string;
}) {
  const [all, setAll] = useState<SecretView[] | null>(null);
  const [err, setErr] = useState('');
  const link = useContext(AccessSecretsLink);

  const load = useCallback(() => api.listSecrets()
    .then(l => { setErr(''); setAll(l); })
    .catch(e => { setErr(e instanceof Error ? e.message : String(e)); }), []);
  // Read on mount, and again each time the server pages close: a picker kept
  // mounted under them (a draft wizard) sees the secret just added there.
  useEffect(() => { if (!link.serverOpen) load(); }, [link.serverOpen, load]);

  const host = remoteHost(url);
  const fitting = (all ?? []).filter(s => secretFits(s, url));
  const chosen = fitting.find(s => s.id === value) ?? null;
  const first = fitting[0] ?? null;

  // Keep the value one of the fitting secrets: the first when no fitting one
  // is chosen, '' when none fit.
  useEffect(() => {
    if (all === null) return;
    if (first && !chosen) onChange(first.id, first);
    else if (!first && value !== '') onChange('', null);
  }, [all, first, chosen, value, onChange]);

  return (
    <div data-testid={testid}>
      {err && <div role="alert" data-testid={`${testid}-error`} style={wizardErr}>{err}</div>}
      {all === null && !err && <div style={fieldHint}>Loading stored secrets…</div>}
      {all !== null && first && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <select data-testid={`${testid}-select`} aria-label="Stored secret" style={{ ...fieldInput, maxWidth: 360 }}
            value={(chosen ?? first).id}
            onChange={e => onChange(e.target.value, fitting.find(s => s.id === e.target.value) ?? null)}>
            {fitting.map(s => (
              <option key={s.id} value={s.id}>
                {s.name}{s.status === 'expired' ? ' (expired)' : s.status === 'expiring' ? ' (expiring)' : ''}
              </option>
            ))}
          </select>
          {chosen && <CredentialChip status={chosen.status} name={chosen.name} />}
        </div>
      )}
      {all !== null && !first && (
        <div style={fieldHint} data-testid={`${testid}-none`}>
          {host ? `No stored secret is allowed for ${host}. Add one in ` : 'Enter a remote URL first. Stored secrets are added in '}
          {link.open
            ? <button type="button" data-testid={`${testid}-access-link`} onClick={link.open}
                style={{ ...linkBtn, fontSize: 'inherit', fontFamily: 'inherit' }}>Access → Secrets</button>
            : 'Access → Secrets'}
          .
        </div>
      )}
    </div>
  );
}
