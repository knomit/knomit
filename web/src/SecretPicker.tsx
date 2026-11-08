import { useCallback, useContext, useEffect, useRef, useState } from 'react';
import { api, type SecretType, type SecretView } from './api';
import { fieldInput, fieldHint, linkBtn, wizardErr } from './manageStyles';
import { CredentialChip } from './CredentialChip';
import { AccessSecretsLink } from './accessLink';
import { remoteHost, secretFits, typesForURL } from './secrets';

// SecretPicker chooses the stored secret a remote uses. It lists ONLY the
// secrets that fit the remote (a type that fits the URL's scheme, and a hosts
// list that allows its host); the server checks the fit again when it links.
// It never creates a secret: that happens only in Access → Secrets.
//
// The value is always '' or a fitting secret:
// - A chosen secret (picked by the reader, or the value the caller started
//   with) that stops fitting is cleared, with a line saying why, and comes
//   back by itself if it fits again (a URL being typed passes through hosts
//   that fit nothing). It is never swapped for another secret.
// - With `preselect`, and only while nothing is chosen and the reader never
//   picked, the first fitting secret that has not expired is chosen (an
//   expired one only when nothing else fits). Only the wizards ask for this:
//   they still test before anything is saved. A surface whose confirm saves
//   at once (the credential box, Restore, Update access) starts empty.
// - None fit: there is no select, only a line naming the kind of secret the
//   URL needs, pointing at Access → Secrets where the server has that page.

const KIND: Record<string, string> = {
  ssh_key: 'SSH-key',
  'token,basic': 'token or username + password',
};

/** The kind of secret a URL takes, in words; '' when it takes none. */
function kindFor(types: SecretType[]): string {
  return KIND[types.join(',')] ?? '';
}

const lostStyle = { fontSize: 12, color: '#e8c98a', margin: '0 0 6px', lineHeight: 1.5 };

export function SecretPicker({ url, value, onChange, preselect = false, testid = 'secret-picker' }: {
  /** The remote the secret is for. */
  url: string;
  /** The chosen secret's id, '' for none. */
  value: string;
  onChange: (id: string, secret: SecretView | null) => void;
  /** Choose a fitting secret by itself while nothing is chosen (see above). */
  preselect?: boolean;
  testid?: string;
}) {
  const [all, setAll] = useState<SecretView[] | null>(null);
  const [err, setErr] = useState('');
  // The secret the reader chose (or the caller started with), '' for none.
  // It outlives the value, which is cleared while it does not fit.
  const [pick, setPick] = useState(value);
  const link = useContext(AccessSecretsLink);

  const load = useCallback(() => api.listSecrets()
    .then(l => { setErr(''); setAll(l); })
    .catch(e => { setErr(e instanceof Error ? e.message : String(e)); }), []);
  // Read on mount, and again each time the server pages close: a picker kept
  // mounted under them sees the secret just added there.
  useEffect(() => { load(); }, [load, link.returns]);

  const host = remoteHost(url);
  const fitting = (all ?? []).filter(s => secretFits(s, url));
  const chosen = fitting.find(s => s.id === value) ?? null;
  const kept = pick ? fitting.find(s => s.id === pick) ?? null : null;
  const best = fitting.find(s => s.status !== 'expired') ?? fitting[0] ?? null;
  const next = kept ?? (preselect && pick === '' ? best : null);

  useEffect(() => {
    if (all === null) return;
    if (value !== '' && !chosen) onChange('', null);
    else if (value === '' && next) onChange(next.id, next);
  }, [all, value, chosen, next, onChange]);

  // Why the reader's choice is not in force.
  const lost = all !== null && pick !== '' && !kept ? all.find(s => s.id === pick) ?? null : null;
  const lostWhy = all === null || pick === '' || kept ? ''
    : lost ? `${lost.name} isn't allowed for ${host || 'this URL'}; pick another.`
      : 'The chosen stored secret no longer exists; pick another.';

  const kind = kindFor(typesForURL(url));

  // Focus came back to the "Access → Secrets" link, and the re-read then
  // replaced the link with a select: the select takes the focus, rather than
  // the page dropping it.
  const selectRef = useRef<HTMLSelectElement>(null);
  const linkFocused = useRef(false);
  const hasSelect = all !== null && fitting.length > 0;
  useEffect(() => {
    if (!hasSelect || !linkFocused.current) return;
    linkFocused.current = false;
    const a = document.activeElement;
    if (!a || a === document.body) selectRef.current?.focus();
  }, [hasSelect]);

  return (
    <div data-testid={testid} data-return-focus="">
      {err && <div role="alert" data-testid={`${testid}-error`} style={wizardErr}>{err}</div>}
      {all === null && !err && <div style={fieldHint}>Loading stored secrets…</div>}
      {lostWhy && <div data-testid={`${testid}-lost`} style={lostStyle}>{lostWhy}</div>}
      {hasSelect && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <select ref={selectRef} data-testid={`${testid}-select`} aria-label="Stored secret" style={{ ...fieldInput, maxWidth: 360 }}
            value={chosen ? chosen.id : ''}
            onChange={e => {
              const s = fitting.find(x => x.id === e.target.value) ?? null;
              setPick(s ? s.id : '');
              onChange(s ? s.id : '', s);
            }}>
            {/* Shown only while nothing is chosen, and never choosable. */}
            {!chosen && <option value="" disabled>Choose a stored secret…</option>}
            {fitting.map(s => (
              <option key={s.id} value={s.id}>
                {s.name}{s.status === 'expired' ? ' (expired)' : s.status === 'expiring' ? ' (expiring)' : ''}
              </option>
            ))}
          </select>
          {chosen && <CredentialChip status={chosen.status} name={chosen.name} />}
        </div>
      )}
      {all !== null && fitting.length === 0 && (
        <div style={fieldHint} data-testid={`${testid}-none`}>
          {!host ? 'Enter a remote URL first.'
            : kind ? `No ${kind} secret is allowed for ${host}.`
              : 'A stored secret cannot be used with this URL.'}
          {host && kind && link.exists && (
            <>
              {' Add one in '}
              {link.open
                ? <button type="button" data-testid={`${testid}-access-link`} onClick={e => link.open?.(e.currentTarget)}
                    onFocus={() => { linkFocused.current = true; }} onBlur={e => { if (e.relatedTarget) linkFocused.current = false; }}
                    style={{ ...linkBtn, fontSize: 'inherit', fontFamily: 'inherit' }}>Access → Secrets</button>
                : 'Access → Secrets'}
              .
            </>
          )}
        </div>
      )}
    </div>
  );
}
