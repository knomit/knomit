import { useState } from 'react';
import { api, type OriginResponse } from './api';
import { btn, confirmBox, linkBtn, segGroup, segment, segDot, warnBox, wizardErr } from './manageStyles';
import { CredentialChip } from './CredentialChip';
import { SecretPicker } from './SecretPicker';
import { PastedCredentialFields } from './PastedCredentialFields';
import { emptyPasted, pastedBody, pastedReady, type PastedCredential } from './secrets';
import { isHTTPURL, isSSHURL } from './originAuth';

// RemoteCredential is the Remote card's Credential row: what the origin
// authenticates with, and the ways to change it WITHOUT reconnecting
// (PUT /origin/credential: no probe, no restart; the next sync uses it).
//
//  - Pasted credential: Replace token… (http(s) remotes) and Use a stored
//    secret….
//  - Stored secret: its name and expiry chip, Use another secret…, and
//    Unlink…, which ALWAYS asks what replaces the link — a pasted credential
//    or explicitly none — with nothing preselected, so a repo is never left
//    anonymous by accident.
//
// Linking and unlinking need the admin gate; the server's refusal is shown
// as it comes.

type Mode = null | 'replace' | 'link' | 'unlink';

export function RemoteCredential({ repo, origin, readOnly, onChanged }: {
  repo: string;
  origin: OriginResponse;
  readOnly: boolean;
  /** The credential changed: re-read the origin. */
  onChanged: () => void;
}) {
  const [mode, setMode] = useState<Mode>(null);
  const [pasted, setPasted] = useState<PastedCredential>(() => emptyPasted(origin.url));
  const [secretId, setSecretId] = useState('');
  const [replacement, setReplacement] = useState<'' | 'pasted' | 'none'>('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const url = origin.url;
  const linked = !!(origin.secret_name || origin.secret_id);
  // A fleet peer authenticates with this instance's certificate; a local
  // path has no credential at all. Neither has anything to change here.
  const remoteKind = isHTTPURL(url) ? 'http' : isSSHURL(url) ? 'ssh' : null;
  if (origin.auth_method === 'cert' || remoteKind === null) return null;

  const open = (m: Mode) => {
    setMode(m); setErr(''); setSecretId(''); setReplacement(''); setPasted(emptyPasted(url));
  };
  const send = async (body: Parameters<typeof api.setOriginCredential>[1]) => {
    setBusy(true); setErr('');
    try {
      await api.setOriginCredential(repo, body);
      setMode(null); setPasted(emptyPasted(url));
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const what = linked
    ? <>Stored secret: <span data-testid="credential-secret-name" style={{ fontFamily: 'var(--k-font-mono)', color: '#ddd' }}>{origin.secret_name || '(unreadable)'}</span></>
    : origin.auth_method === 'none' || origin.auth_method === ''
      ? 'No credential'
      : origin.auth_method === 'ssh' ? "This server's SSH key" : 'Pasted credential';

  const unlinkReady = replacement === 'none' || (replacement === 'pasted' && pastedReady(pasted));

  return (
    <div data-testid="remote-credential" style={{ marginTop: 6 }}>
      <div style={{ fontSize: 12, color: '#888', display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
        <span data-testid="credential-kind">Credential: {what}</span>
        {linked && <CredentialChip status={origin.secret_status} name={origin.secret_name} testid="credential-chip" />}
        {!readOnly && mode === null && (
          <>
            {!linked && remoteKind === 'http' && (
              <button type="button" data-testid="credential-replace" style={linkBtn} onClick={() => open('replace')}>Replace token…</button>
            )}
            <button type="button" data-testid="credential-link" style={linkBtn} onClick={() => open('link')}>
              {linked ? 'Use another secret…' : 'Use a stored secret…'}
            </button>
            {linked && (
              <button type="button" data-testid="credential-unlink" style={linkBtn} onClick={() => open('unlink')}>Unlink…</button>
            )}
          </>
        )}
      </div>

      {linked && (origin.secret_status === 'expiring' || origin.secret_status === 'expired') && (
        <div data-testid="credential-warning" style={warnBox}>
          {origin.secret_status === 'expired'
            ? <>The stored secret {origin.secret_name} has expired. Sync keeps trying with it; replace its value in Access → Secrets.</>
            : <>The stored secret {origin.secret_name} expires within 14 days. Replace its value in Access → Secrets.</>}
        </div>
      )}

      {mode === 'replace' && (
        <div style={confirmBox} data-testid="credential-replace-box">
          <div style={{ fontSize: 13, marginBottom: 4 }}>New credential for this remote. Sync uses it right away; nothing reconnects.</div>
          <PastedCredentialFields url={url} value={pasted} onChange={setPasted} testid="credential-pasted" />
          <Actions busy={busy} ready={pastedReady(pasted)} label="Save" onCancel={() => setMode(null)}
            onConfirm={() => send(pastedBody(pasted))} testid="credential-replace-save" />
        </div>
      )}

      {mode === 'link' && (
        <div style={confirmBox} data-testid="credential-link-box">
          <div style={{ fontSize: 13, marginBottom: 8 }}>
            {linked ? 'Switch to another stored secret.' : 'Use a stored secret instead of the pasted credential. The pasted credential is removed.'}
          </div>
          <SecretPicker url={url} value={secretId} onChange={id => setSecretId(id)} allowCreate testid="credential-picker" />
          <Actions busy={busy} ready={secretId !== ''} label="Use this secret" onCancel={() => setMode(null)}
            onConfirm={() => send({ secret_id: secretId })} testid="credential-link-save" />
        </div>
      )}

      {mode === 'unlink' && (
        <div style={confirmBox} data-testid="credential-unlink-box">
          <div style={{ fontSize: 13, marginBottom: 8 }}>What should this remote use instead of {origin.secret_name || 'the stored secret'}?</div>
          <div role="radiogroup" aria-label="Replacement credential" style={segGroup}>
            <button type="button" role="radio" aria-checked={replacement === 'pasted'} data-testid="unlink-choice-pasted"
              style={segment(replacement === 'pasted', 'remote')} onClick={() => setReplacement('pasted')}>
              <span style={segDot(replacement === 'pasted', 'remote')} />
              <span>{remoteKind === 'ssh' ? "This server's SSH key" : 'A pasted credential'}</span>
            </button>
            <button type="button" role="radio" aria-checked={replacement === 'none'} data-testid="unlink-choice-none"
              style={segment(replacement === 'none', 'neutral')} onClick={() => setReplacement('none')}>
              <span style={segDot(replacement === 'none', 'neutral')} />
              <span>No credential (public remote)</span>
            </button>
          </div>
          {replacement === 'pasted' && remoteKind === 'http' && (
            <PastedCredentialFields url={url} value={pasted} onChange={setPasted} testid="unlink-pasted" />
          )}
          <Actions busy={busy} ready={unlinkReady} label="Unlink" danger onCancel={() => setMode(null)}
            onConfirm={() => send(replacement === 'none' ? { secret_id: null } : { secret_id: null, ...pastedBody(pasted) })}
            testid="credential-unlink-confirm" />
        </div>
      )}

      {err && <div role="alert" data-testid="credential-error" style={wizardErr}>{err}</div>}
    </div>
  );
}

function Actions({ busy, ready, label, danger, onConfirm, onCancel, testid }: {
  busy: boolean; ready: boolean; label: string; danger?: boolean;
  onConfirm: () => void; onCancel: () => void; testid: string;
}) {
  return (
    <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
      <button type="button" data-testid={testid} style={btn(busy || !ready, danger ? 'danger' : 'primary')}
        disabled={busy || !ready} onClick={onConfirm}>{busy ? 'Saving…' : label}</button>
      <button type="button" style={btn(busy)} disabled={busy} onClick={onCancel}>Cancel</button>
    </div>
  );
}
