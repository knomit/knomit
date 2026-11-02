import { useCallback, useEffect, useState, type CSSProperties } from 'react';
import { api, type SecretView } from './api';
import { btn, fieldInput, fieldHint, linkBtn, wizardErr, confirmBox } from './manageStyles';
import { SettingsPage, type Section } from './SettingsPage';
import { CredentialChip } from './CredentialChip';
import { SecretForm } from './SecretForm';
import { SECRET_TYPE_LABEL, expiryDate, expiryFromDate, parseHosts } from './secrets';

// SecretsTab is Access → Secrets: the instance's stored credentials, which
// repos link to instead of holding a pasted token. One table of every secret
// (name, type, allowed hosts, expiry, status, linked repos) and, under it,
// the editor of the one being changed. Values are write-only: nothing here
// ever shows one, and Replace value is the only way to change it.

const dim: CSSProperties = { color: '#888', fontSize: 12 };
const errMsg = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function SecretsTab() {
  const [list, setList] = useState<SecretView[] | null>(null);
  const [err, setErr] = useState('');
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [focus, setFocus] = useState<string | undefined>(undefined);

  const load = useCallback(() => {
    api.listSecrets()
      .then(l => { setErr(''); setList(l); })
      .catch(e => setErr(errMsg(e)));
  }, []);
  useEffect(() => { load(); }, [load]);

  const edit = (id: string) => { setEditing(id); setAdding(false); setFocus('secret-edit'); };
  const current = list?.find(s => s.id === editing) ?? null;

  const table = list !== null && list.length > 0 && (
    <table data-testid="secrets-table" style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13, marginTop: 10 }}>
      <thead>
        <tr>
          {['Name', 'Type', 'Hosts', 'Expires', 'Status', 'Repos', ''].map(h => <th key={h} style={th}>{h}</th>)}
        </tr>
      </thead>
      <tbody>
        {list.map(s => (
          <tr key={s.id} data-testid={`secret-row-${s.name}`} style={{ borderTop: '1px solid #1e1e1e', background: s.id === editing ? '#14161a' : undefined }}>
            <td style={{ ...td, color: '#eee', fontFamily: 'var(--k-font-mono)' }}>{s.name}</td>
            <td style={td}>{SECRET_TYPE_LABEL[s.type] ?? s.type}</td>
            <td style={{ ...td, fontFamily: 'var(--k-font-mono)', fontSize: 12 }}>{s.hosts.join(', ')}</td>
            <td style={td}>{s.expires_at ? expiryDate(s.expires_at) : '—'}</td>
            <td style={td}><CredentialChip status={s.status} name={s.name} testid={`secret-status-${s.name}`} />{(s.status === 'ok' || s.status === 'none') && <span style={dim}>{s.status === 'ok' ? 'ok' : '—'}</span>}</td>
            <td style={{ ...td, fontVariantNumeric: 'tabular-nums' }} data-testid={`secret-linked-${s.name}`}>{s.linked_repos}</td>
            <td style={{ ...td, textAlign: 'right' }}>
              <button type="button" style={linkBtn} data-testid={`secret-edit-${s.name}`} onClick={() => edit(s.id)}>Edit</button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );

  const sections: Section[] = [{
    id: 'secrets',
    title: 'Stored secrets',
    action: (
      <button type="button" data-testid="secret-add" style={btn(adding)} disabled={adding}
        onClick={() => { setAdding(true); setEditing(null); }}>Add secret</button>
    ),
    body: (
      <>
        <div style={dim}>
          Credentials repositories can use instead of a pasted token. Replace a value once here and every
          repository using it picks it up. Values are never shown again after you save them.
        </div>
        {err && <div role="alert" data-testid="secrets-error" style={wizardErr}>{err}</div>}
        {adding && (
          <div style={confirmBox}>
            <SecretForm mode="create" onCancel={() => setAdding(false)}
              onDone={v => { setAdding(false); load(); edit(v.id); }} />
          </div>
        )}
        {list === null && !err && <div style={{ ...dim, marginTop: 10 }}>Loading…</div>}
        {list !== null && list.length === 0 && <div style={{ ...dim, marginTop: 10 }} data-testid="secrets-empty">No stored secrets yet.</div>}
        {table}
      </>
    ),
  }];
  if (current) {
    sections.push({
      id: 'secret-edit',
      title: current.name,
      tail: <CredentialChip status={current.status} name={current.name} compact />,
      body: <SecretEditor key={current.id} secret={current} onChanged={load}
        onDeleted={() => { setEditing(null); load(); }} />,
    });
  }

  return <SettingsPage kind="secrets" sections={sections} focus={focus} onFocused={() => setFocus(undefined)} testid="secrets-tab" />;
}

// SecretEditor changes one secret: its name, expiry, allowed hosts and value,
// and deletes it once no repository uses it.
function SecretEditor({ secret, onChanged, onDeleted }: {
  secret: SecretView;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  const [name, setName] = useState(secret.name);
  const [expiry, setExpiry] = useState(expiryDate(secret.expires_at));
  const [hosts, setHosts] = useState<string[]>(secret.hosts);
  const [newHost, setNewHost] = useState('');
  const [replacing, setReplacing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [repos, setRepos] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [note, setNote] = useState('');

  useEffect(() => {
    let cancelled = false;
    api.getSecret(secret.id)
      .then(d => { if (!cancelled) setRepos((d.repos ?? []).map(r => r.name)); })
      .catch(() => { if (!cancelled) setRepos(null); });
    return () => { cancelled = true; };
  }, [secret.id, secret.linked_repos]);

  const run = async (f: () => Promise<unknown>, done: string, onFail?: () => void) => {
    setBusy(true); setErr(''); setNote('');
    try {
      await f();
      setNote(done);
      onChanged();
    } catch (e) {
      // A refusal's detail names what blocks it (the repos using a host).
      setErr(errMsg(e));
      onFail?.();
    } finally {
      setBusy(false);
    }
  };

  const hostsChanged = hosts.join('\u0000') !== secret.hosts.join('\u0000');
  const linked = secret.linked_repos > 0;
  const deleteWhy = linked
    ? `Used by ${secret.linked_repos} ${secret.linked_repos === 1 ? 'repository' : 'repositories'}${repos && repos.length ? `: ${repos.join(', ')}` : ''}. Unlink ${secret.linked_repos === 1 ? 'it' : 'them'} first.`
    : '';

  return (
    <div data-testid="secret-editor">
      <div style={dim}>
        {SECRET_TYPE_LABEL[secret.type]}{secret.fingerprint ? <> · <span style={{ fontFamily: 'var(--k-font-mono)' }}>{secret.fingerprint}</span></> : null}
        {' · '}
        {linked ? <span data-testid="secret-linked-repos">Used by {repos ? repos.join(', ') : `${secret.linked_repos} repositories`}</span> : 'Not used by any repository'}
      </div>

      <div style={row}>
        <span style={rowLabel}>Name</span>
        <input data-testid="secret-rename" style={{ ...fieldInput, maxWidth: 260 }} value={name} onChange={e => setName(e.target.value)} />
        <button type="button" data-testid="secret-rename-save" style={btn(busy || name.trim() === '' || name === secret.name)}
          disabled={busy || name.trim() === '' || name === secret.name}
          onClick={() => run(() => api.patchSecret(secret.id, { name: name.trim() }), 'Renamed.')}>Rename</button>
      </div>

      <div style={row}>
        <span style={rowLabel}>Expires</span>
        <input data-testid="secret-expiry-edit" type="date" style={{ ...fieldInput, maxWidth: 180 }} value={expiry} onChange={e => setExpiry(e.target.value)} />
        <button type="button" data-testid="secret-expiry-save" style={btn(busy || expiry === expiryDate(secret.expires_at))}
          disabled={busy || expiry === expiryDate(secret.expires_at)}
          onClick={() => run(() => api.patchSecret(secret.id, { expires_at: expiryFromDate(expiry) }), expiry ? 'Expiry saved.' : 'Expiry cleared.')}>Save</button>
        {secret.expires_at && (
          <button type="button" style={linkBtn} disabled={busy}
            onClick={() => { setExpiry(''); run(() => api.patchSecret(secret.id, { expires_at: null }), 'Expiry cleared.'); }}>No expiry</button>
        )}
      </div>

      <div style={{ ...row, alignItems: 'flex-start' }}>
        <span style={{ ...rowLabel, paddingTop: 4 }}>Hosts</span>
        <div style={{ flex: 1 }}>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {hosts.map(h => (
              <span key={h} data-testid={`secret-host-${h}`} style={hostChip}>
                {h}
                <button type="button" aria-label={`Remove ${h}`} data-testid={`secret-host-remove-${h}`}
                  disabled={busy || hosts.length === 1}
                  title={hosts.length === 1 ? 'A secret needs at least one host' : `Remove ${h}`}
                  style={{ background: 'none', border: 'none', color: '#888', cursor: hosts.length === 1 ? 'default' : 'pointer', padding: '0 0 0 6px' }}
                  onClick={() => setHosts(hosts.filter(x => x !== h))}>×</button>
              </span>
            ))}
          </div>
          <div style={{ display: 'flex', gap: 6, marginTop: 6 }}>
            <input data-testid="secret-host-new" style={{ ...fieldInput, maxWidth: 220 }} value={newHost} placeholder="add a host"
              onChange={e => setNewHost(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addHost(); } }} />
            <button type="button" data-testid="secret-host-add" style={btn(busy || newHost.trim() === '')}
              disabled={busy || newHost.trim() === ''} onClick={addHost}>Add</button>
            <button type="button" data-testid="secret-hosts-save" style={btn(busy || !hostsChanged, 'primary')}
              disabled={busy || !hostsChanged}
              onClick={() => run(() => api.patchSecret(secret.id, { hosts }), 'Hosts saved.', () => setHosts(secret.hosts))}>Save hosts</button>
          </div>
          <div style={fieldHint}>A host still used by a linked repository cannot be removed.</div>
        </div>
      </div>

      <div style={row}>
        <span style={rowLabel}>Value</span>
        {!replacing && (
          <button type="button" data-testid="secret-replace" style={btn(busy)} disabled={busy}
            onClick={() => setReplacing(true)}>Replace value…</button>
        )}
      </div>
      {replacing && (
        <div style={confirmBox}>
          <SecretForm mode="replace" secret={secret} onCancel={() => setReplacing(false)}
            onDone={() => { setReplacing(false); setNote('Value replaced. Linked repositories use it on their next sync.'); onChanged(); }} />
        </div>
      )}

      <div style={{ ...row, marginTop: 18 }}>
        <span style={rowLabel} />
        {!deleting ? (
          <span title={deleteWhy || undefined}>
            <button type="button" data-testid="secret-delete" style={btn(busy || linked, 'danger')}
              disabled={busy || linked} onClick={() => setDeleting(true)}>Delete…</button>
          </span>
        ) : (
          <>
            <span style={dim}>Delete {secret.name}? This cannot be undone.</span>
            <button type="button" data-testid="secret-delete-confirm" style={btn(busy, 'danger')} disabled={busy}
              onClick={async () => {
                setBusy(true); setErr('');
                try { await api.deleteSecret(secret.id); onDeleted(); } catch (e) { setErr(errMsg(e)); setBusy(false); setDeleting(false); }
              }}>Delete</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setDeleting(false)}>Cancel</button>
          </>
        )}
      </div>
      {linked && <div data-testid="secret-delete-why" style={fieldHint}>{deleteWhy}</div>}

      {err && <div role="alert" data-testid="secret-editor-error" style={wizardErr}>{err}</div>}
      {note && !err && <div data-testid="secret-editor-note" style={{ ...dim, marginTop: 10, color: '#7c9' }}>{note}</div>}
    </div>
  );

  function addHost() {
    const add = parseHosts(newHost).filter(h => !hosts.includes(h));
    if (add.length > 0) setHosts([...hosts, ...add]);
    setNewHost('');
  }
}

const th: CSSProperties = {
  textAlign: 'left', fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.2, color: '#555',
  fontWeight: 400, padding: '4px 8px 6px 0',
};
const td: CSSProperties = { padding: '7px 8px 7px 0', color: '#bbb', verticalAlign: 'middle' };
const row: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, marginTop: 12, flexWrap: 'wrap' };
const rowLabel: CSSProperties = { width: 70, color: '#888', fontSize: 12 };
const hostChip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', fontFamily: 'var(--k-font-mono)', fontSize: 12,
  padding: '2px 4px 2px 8px', borderRadius: 4, background: '#1a1a1a', border: '1px solid #2a2a2a', color: '#ccc',
};
