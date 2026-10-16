import { useState, type CSSProperties, type ReactNode } from 'react';
import { CopyIcon } from './icons';
import { fieldHint, fieldInput, fieldLabel, wizardErr, wizardErrNote } from './manageStyles';
import type { FleetAuthMethod } from './fleetWizardState';
import { CHIP_TONES, type ChipTone } from './fleetText';
import type { FleetRefusal } from './api';

// The fleet pages' small parts, one rendering each: a chip, a copy button, a
// key/value grid, and a refusal (the server's words, its code beneath, then
// what was kept).

/** Chip: the rail's chip geometry in a fleet tone. */
export function Chip({ text, tone, testid }: { text: string; tone: ChipTone; testid?: string }) {
  return <span data-testid={testid} data-tone={tone} style={{ ...chipBase, ...CHIP_TONES[tone] }}>{text}</span>;
}

/** CopyButton is the app's copy affordance (CopyIcon, green for 1.5 s after
 *  a copy): navigator.clipboard.writeText of exactly `text`. */
export function CopyButton({ text, label, testid }: { text: string; label: string; testid?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(text)
      .then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); })
      .catch(() => { /* clipboard unavailable: nothing to recover */ });
  };
  return (
    <button type="button" data-testid={testid} title={copied ? 'Copied' : 'Copy'} aria-label={label} style={copyBtn} onClick={copy}>
      <CopyIcon color={copied ? '#7c9' : '#888'} size={13} />
    </button>
  );
}

/** CopyValue: a mono value with its copy button beside it. */
export function CopyValue({ text, label, testid }: { text: string; label: string; testid?: string }) {
  return (
    <span style={copyRow}>
      <span data-testid={testid} style={mono}>{text}</span>
      <CopyButton text={text} label={label} />
    </span>
  );
}

/** Kv: the settings blocks' key/value grid. A row with a null value is left out. */
export function Kv({ rows, testid }: { rows: [string, ReactNode | null][]; testid?: string }) {
  return (
    <div data-testid={testid} style={kvGrid}>
      {rows.filter(([, v]) => v !== null).map(([k, v]) => (
        <div key={k} style={kvRow}>
          <span style={kvKey}>{k}</span>
          <span style={kvVal}>{v}</span>
        </div>
      ))}
    </div>
  );
}

/** Refusal: a refused action under its button. The server's message
 *  verbatim, its code beneath (the repo wizard's error box), then the note
 *  that says what was (not) kept. */
export function Refusal({ err, note, testid = 'fleet-refusal' }: { err: Error | null; note?: string; testid?: string }) {
  if (!err) return null;
  const code = (err as FleetRefusal).code;
  return (
    <div data-testid={testid} role="alert" style={wizardErr}>
      <div>{err.message}</div>
      {code && <div data-testid={`${testid}-code`} style={codeLine}>{code}</div>}
      {note && <div style={wizardErrNote}>{note}</div>}
    </div>
  );
}

/** CredentialFields is the forge credential of the fleet's Access steps
 *  (New fleet → Access, and Settings' Point at / Update access / Move): the
 *  repo Connect wizard's method, username and token fields, with the same
 *  rule for which appear (no token for ssh, none or an SSH URL under
 *  auto-detect). hint sits under the method, where the repo step puts it. */
export function CredentialFields({ url, method, user, token, onMethod, onUser, onToken, hint, idPrefix = 'fleet', disabled }: {
  url: string;
  method: FleetAuthMethod;
  user: string;
  token: string;
  onMethod: (m: FleetAuthMethod) => void;
  onUser: (u: string) => void;
  onToken: (t: string) => void;
  hint?: ReactNode;
  idPrefix?: string;
  disabled?: boolean;
}) {
  const ssh = /^[^@/]+@[^:]+:|^ssh:\/\//.test(url);
  return (
    <>
      <label style={fieldLabel} htmlFor={`${idPrefix}-auth-method`}>Auth method</label>
      <select id={`${idPrefix}-auth-method`} data-testid={`${idPrefix}-auth-method`} style={fieldInput} value={method} disabled={disabled}
        onChange={e => onMethod(e.target.value as FleetAuthMethod)}>
        <option value="">auto-detect</option>
        <option value="none">none</option>
        <option value="token">token</option>
        <option value="basic">basic</option>
        <option value="ssh">ssh</option>
      </select>
      {hint && <div style={fieldHint}>{hint}</div>}
      {method === 'basic' && (
        <>
          <label style={fieldLabel} htmlFor={`${idPrefix}-auth-user`}>Username</label>
          <input id={`${idPrefix}-auth-user`} data-testid={`${idPrefix}-auth-user`} style={fieldInput} value={user} placeholder="username" disabled={disabled}
            onChange={e => onUser(e.target.value)} />
        </>
      )}
      {method !== 'ssh' && method !== 'none' && !(ssh && method === '') && (
        <>
          <label style={fieldLabel} htmlFor={`${idPrefix}-auth-token`}>{method === 'basic' ? 'Password' : 'Token'}</label>
          <input id={`${idPrefix}-auth-token`} data-testid={`${idPrefix}-auth-token`} type="password" style={fieldInput} value={token} disabled={disabled}
            placeholder="kept in knomit's credential store" onChange={e => onToken(e.target.value)} />
        </>
      )}
    </>
  );
}

const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, overflowWrap: 'anywhere' };


const chipBase: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', fontSize: 10, lineHeight: 1.7, padding: '0 6px',
  borderRadius: 3, fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};
const copyBtn: CSSProperties = { background: 'none', border: 'none', padding: 2, cursor: 'pointer', display: 'inline-flex', alignItems: 'center', flexShrink: 0 };
const copyRow: CSSProperties = { display: 'inline-flex', alignItems: 'center', gap: 6, maxWidth: '100%', minWidth: 0 };
const kvGrid: CSSProperties = { display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 };
const kvRow: CSSProperties = { display: 'flex', flexWrap: 'wrap', columnGap: 18, rowGap: 2, alignItems: 'baseline' };
const kvKey: CSSProperties = { color: '#888', fontSize: 12, flex: '0 0 120px' };
const kvVal: CSSProperties = { color: '#ddd', minWidth: 0, flex: '1 1 220px', overflowWrap: 'anywhere' };
const codeLine: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#a08750', marginTop: 3 };
