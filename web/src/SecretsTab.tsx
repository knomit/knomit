import { useEffect, useMemo, useState, type CSSProperties } from 'react';
import { api, type SecretType, type SecretView } from './api';
import { btn, fieldInput, wizardErr } from './manageStyles';
import { SecretForm } from './SecretForm';
import { RepoDot } from './settingsParts';
import { CopyButton } from './fleetParts';
import { NEW_SECRET, errMsg, expiryBadge, expiryDate, expiryFromDate, parseHosts } from './secrets';
import { KeyIcon, TicketIcon, IdCardIcon, TerminalIcon, PencilIcon, TrashIcon, RefreshIcon, PlusIcon } from './icons';
import {
  Badge, Block, Chip, DetailHeader, IconAction, ItemRow, ListEmpty, ListHeader, Note, Prop, Stats,
} from './AccessParts';
import { attnRow, dim, fmtDate, listKeys, listScroll, mono, relDays, tabStop } from './accessUtil';

// Access → Secrets: the instance's stored credentials, which repositories
// link to instead of holding a pasted token. The list column names every
// secret (its kind, its hosts, and a badge when its expiry needs attention);
// the detail column shows one and is where it is changed. Values are
// write-only: nothing here ever shows one, and Replace value is the only way
// to change it.

/** The glyph of a secret's kind: a ticket for a bearer token, an ID card for
 *  a username and password, a prompt for an SSH key. */
export function SecretTypeIcon({ type, color, size = 14 }: { type: SecretType; color: string; size?: number }) {
  if (type === 'basic') return <IdCardIcon color={color} size={size} />;
  if (type === 'ssh_key') return <TerminalIcon color={color} size={size} />;
  return <TicketIcon color={color} size={size} />;
}
const TYPE_COLOR: Record<SecretType, string> = { token: '#8ab6d6', basic: '#c8a8f0', ssh_key: '#8ad6c8' };
const TYPE_WORD: Record<SecretType, string> = { token: 'token', basic: 'username + password', ssh_key: 'ssh key' };

// ─── List ────────────────────────────────────────────────────────────────────

export function SecretsList({ list, err, selected, onSelect, focusOnly, adding, onAdd, onLeft, onRight }: {
  list: SecretView[] | null;
  err: string;
  selected: string | null;
  onSelect: (id: string) => void;
  /** One column at a time: arrows move the focus, Enter opens (listKeys). */
  focusOnly?: boolean;
  adding: boolean;
  onAdd: () => void;
  onLeft: () => void;
  onRight: () => void;
}) {
  const [q, setQ] = useState('');
  const shown = useMemo(() => {
    const t = q.trim().toLowerCase();
    const l = [...(list ?? [])].sort((a, b) => a.name.localeCompare(b.name));
    return t ? l.filter(s => s.name.toLowerCase().includes(t) || s.hosts.some(h => h.includes(t))) : l;
  }, [list, q]);
  const ids = [...(adding ? [NEW_SECRET] : []), ...shown.map(s => s.id)];
  const tab = tabStop(ids, selected);
  return (
    <>
      <ListHeader query={q} onQuery={setQ} placeholder="Find a secret or host"
        add={{ label: 'Add a secret', active: adding, onClick: onAdd, testid: 'secret-add' }} />
      <div data-testid="secrets-list" role="list" style={listScroll} onKeyDown={listKeys(ids, selected, onSelect, { onLeft, onRight, focusOnly })}>
        {err && <div role="alert" data-testid="secrets-error" style={{ ...wizardErr, margin: 12 }}>{err}</div>}
        {adding && (
          <ItemRow id={NEW_SECRET} selected={selected === NEW_SECRET} tabbable={tab === NEW_SECRET} onSelect={() => onSelect(NEW_SECRET)} italic
            glyph={<PlusIcon color="#8ab6d6" size={14} />} title="New secret" meta="not saved yet" testid="secret-row-new" />
        )}
        {list === null && !err && <div style={{ ...dim, padding: 16 }}>Loading…</div>}
        {list !== null && list.length === 0 && !adding && (
          <ListEmpty testid="secrets-empty" icon={<KeyIcon color="#666" size={18} />} title="No stored secrets yet">
            Store a token, a username and password, or an SSH key once, and link repositories to it instead of pasting it into each. Add one with the + button above.
          </ListEmpty>
        )}
        {list !== null && list.length > 0 && shown.length === 0 && (
          <div style={{ ...dim, padding: '16px 14px' }}>Nothing matches “{q.trim()}”.</div>
        )}
        {shown.map(s => {
          const b = expiryBadge(s);
          return (
            <ItemRow key={s.id} id={s.id} selected={s.id === selected} tabbable={tab === s.id} onSelect={() => onSelect(s.id)} titleMono
              testid={`secret-row-${s.name}`}
              glyph={<SecretTypeIcon type={s.type} color={s.id === selected ? TYPE_COLOR[s.type] : '#777'} />}
              title={s.name}
              meta={<>{s.hosts.join(', ')}{' · '}{s.linked_repos === 0 ? 'unused' : `${s.linked_repos} ${s.linked_repos === 1 ? 'repo' : 'repos'}`}</>}
              badge={b && <Badge tone={b.tone} title={b.title} testid={`secret-status-${s.name}`}>{b.text}</Badge>} />
          );
        })}
      </div>
    </>
  );
}

// ─── Overview (nothing picked) ───────────────────────────────────────────────

export function SecretsOverview({ list, onPick }: { list: SecretView[] | null; onPick: (id: string) => void }) {
  const l = list ?? [];
  const attention = l.filter(s => s.status === 'expired' || s.status === 'expiring')
    .sort((a, b) => (a.expires_at ?? '').localeCompare(b.expires_at ?? ''));
  return (
    <div data-testid="secrets-overview">
      <DetailHeader kindIcon={<KeyIcon color="#7c9" size={12} />} kind="secrets" kindColor="#7c9"
        title="Stored credentials"
        sub="Credentials repositories use instead of a pasted token. Replace a value once here and every repository using it picks it up. Values are never shown again after you save them." />
      <div style={{ marginTop: 22 }}>
        <Stats items={[
          { n: l.length, label: 'secrets' },
          { n: l.filter(s => s.linked_repos > 0).length, label: 'in use' },
          { n: l.filter(s => s.status === 'expiring').length, label: 'expiring', tone: 'warn' },
          { n: l.filter(s => s.status === 'expired').length, label: 'expired', tone: 'bad' },
        ]} />
      </div>
      {attention.length > 0 && (
        <Block title="Needs attention">
          {attention.map(s => {
            const b = expiryBadge(s, true)!;
            return (
              <button key={s.id} type="button" onClick={() => onPick(s.id)} style={attnRow}
                onMouseEnter={e => { e.currentTarget.style.background = '#191922'; }}
                onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}>
                <SecretTypeIcon type={s.type} color={TYPE_COLOR[s.type]} size={13} />
                <span style={{ ...mono, fontSize: 12.5, color: '#ddd', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.name}</span>
                <Badge tone={b.tone} title={b.title}>{b.text}</Badge>
                <span style={{ ...dim, flex: 1, textAlign: 'right', whiteSpace: 'nowrap' }}>
                  {s.linked_repos > 0 ? `${s.linked_repos} ${s.linked_repos === 1 ? 'repo' : 'repos'}` : 'unused'}
                </span>
              </button>
            );
          })}
        </Block>
      )}
    </div>
  );
}

// ─── New secret ──────────────────────────────────────────────────────────────

export function SecretCreate({ onCancel, onCreated }: { onCancel: () => void; onCreated: (v: SecretView) => void }) {
  return (
    <div data-testid="secret-create">
      <DetailHeader kindIcon={<PlusIcon color="#8ab6d6" size={12} />} kind="new secret" kindColor="#8ab6d6"
        title="Add a secret"
        sub="A name repositories will know it by, the hosts it may be sent to, and its value. The value is write-only: once saved, it is never shown again." />
      <div style={{ marginTop: 4 }}>
        <SecretForm mode="create" onCancel={onCancel} onDone={onCreated} />
      </div>
    </div>
  );
}

// ─── Detail ──────────────────────────────────────────────────────────────────

type Editing = null | 'name' | 'expiry' | 'hosts' | 'value' | 'delete';

/** SecretDetail shows one secret and changes it: its name, allowed hosts,
 *  expiry and value; and deletes it once no repository uses it. */
export function SecretDetail({ secret, onChanged, onDeleted }: {
  secret: SecretView;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  const [editing, setEditing] = useState<Editing>(null);
  const [name, setName] = useState(secret.name);
  const [expiry, setExpiry] = useState(expiryDate(secret.expires_at));
  const [hosts, setHosts] = useState<string[]>(secret.hosts);
  const [newHost, setNewHost] = useState('');
  const [repos, setRepos] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [note, setNote] = useState('');

  // The list carries only the count; the detail reads which repositories.
  useEffect(() => {
    let cancelled = false;
    api.getSecret(secret.id)
      .then(d => { if (!cancelled) setRepos((d.repos ?? []).map(r => r.name)); })
      .catch(() => { if (!cancelled) setRepos(null); });
    return () => { cancelled = true; };
  }, [secret.id, secret.linked_repos]);

  const begin = (e: Editing) => {
    setEditing(e); setErr(''); setNote('');
    setName(secret.name); setExpiry(expiryDate(secret.expires_at)); setHosts(secret.hosts); setNewHost('');
  };
  const run = async (f: () => Promise<unknown>, done: string) => {
    setBusy(true); setErr(''); setNote('');
    try {
      await f();
      setNote(done);
      setEditing(null);
      onChanged();
    } catch (e) {
      // A refusal's detail names what blocks it (the repos using a host).
      setErr(errMsg(e));
    } finally {
      setBusy(false);
    }
  };
  const addHost = () => {
    const add = parseHosts(newHost).filter(h => !hosts.includes(h));
    if (add.length > 0) setHosts([...hosts, ...add]);
    setNewHost('');
  };
  const rename = () => run(() => api.patchSecret(secret.id, { name: name.trim() }), 'Renamed. Linked repositories follow the new name.');

  const linked = secret.linked_repos > 0;
  const status = expiryBadge(secret, true)!;
  const hostsChanged = hosts.join('\u0000') !== secret.hosts.join('\u0000');
  const repoWord = (n: number) => (n === 1 ? 'repository' : 'repositories');
  const canRename = !busy && name.trim() !== '' && name.trim() !== secret.name;

  return (
    <div data-testid="secret-editor">
      <DetailHeader
        kindIcon={<SecretTypeIcon type={secret.type} color={TYPE_COLOR[secret.type]} size={12} />}
        kind={TYPE_WORD[secret.type]} kindColor={TYPE_COLOR[secret.type]}
        status={<Badge tone={status.tone} title={status.title} dot testid="secret-status">{status.text}</Badge>}
        aside={<>updated {relDays(secret.updated_at)}</>}
        titleMono
        title={editing === 'name' ? (
          <span style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <input data-testid="secret-rename" autoFocus value={name} onChange={e => setName(e.target.value)} aria-label="Name"
              onKeyDown={e => { if (e.key === 'Escape') setEditing(null); if (e.key === 'Enter' && canRename) rename(); }}
              style={{ ...fieldInput, ...mono, fontSize: 16, maxWidth: 300, padding: '4px 8px' }} />
            <button type="button" data-testid="secret-rename-save" style={btn(!canRename, 'primary')} disabled={!canRename} onClick={rename}>Rename</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setEditing(null)}>Cancel</button>
          </span>
        ) : secret.name}
        actions={editing !== 'name' && (
          <IconAction testid="secret-rename-start" icon={<PencilIcon color="currentColor" size={12} />} label="Rename" onClick={() => begin('name')} />
        )}
        sub={<>{linked ? `Used by ${secret.linked_repos} ${repoWord(secret.linked_repos)}` : 'Not used by any repository'} · created {fmtDate(secret.created_at)}</>}
      />

      {err && <div role="alert" data-testid="secret-editor-error" style={wizardErr}>{err}</div>}
      {note && !err && <Note tone="ok" testid="secret-editor-note" onDismiss={() => setNote('')}>{note}</Note>}

      <Block title="Credential">
        <Prop label="Value" top={editing === 'value'}
          action={editing !== 'value' && <IconAction testid="secret-replace" icon={<RefreshIcon color="currentColor" size={12} />} label="Replace…" onClick={() => begin('value')} />}>
          {editing === 'value' ? (
            <div style={{ marginTop: -12 }}>
              <SecretForm mode="replace" secret={secret} onCancel={() => setEditing(null)}
                onDone={() => { setEditing(null); setNote('Value replaced. Linked repositories use it on their next sync.'); onChanged(); }} />
            </div>
          ) : (
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
              <span aria-label="hidden value" style={{ ...mono, color: '#666' }}>••••••••••••••••</span>
              <span style={{ ...dim, fontSize: 11 }}>write-only</span>
            </span>
          )}
        </Prop>
        {secret.fingerprint && (
          <Prop label="Fingerprint">
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, minWidth: 0, maxWidth: '100%' }}>
              <span data-testid="secret-fingerprint" style={{ ...mono, fontSize: 12, color: '#bbb', overflowWrap: 'anywhere' }}>{secret.fingerprint}</span>
              <CopyButton text={secret.fingerprint} label="Copy the fingerprint" />
            </span>
          </Prop>
        )}
      </Block>

      <Block title="Where it may be used">
        <Prop label="Allowed hosts" top
          action={editing !== 'hosts' && <IconAction testid="secret-hosts-edit" icon={<PencilIcon color="currentColor" size={12} />} label="Edit" onClick={() => begin('hosts')} />}>
          {editing !== 'hosts' ? (
            <span style={{ display: 'flex', flexWrap: 'wrap', gap: 6, paddingTop: 1 }}>
              {secret.hosts.map(h => <Chip key={h} testid={`secret-host-${h}`}>{h}</Chip>)}
            </span>
          ) : (
            <div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                {[...secret.hosts, ...hosts.filter(h => !secret.hosts.includes(h))].map(h => {
                  const kept = hosts.includes(h);
                  const added = !secret.hosts.includes(h);
                  return (
                    <Chip key={h} testid={`secret-host-${h}`} tone={added ? 'add' : kept ? undefined : 'drop'}
                      onRemove={kept ? () => setHosts(hosts.filter(x => x !== h)) : () => setHosts([...hosts, h])}
                      removeLabel={kept ? `Remove ${h}` : `Keep ${h}`}
                      removeDisabled={busy || (kept && hosts.length === 1)}
                      removeTitle={kept && hosts.length === 1 ? 'A secret needs at least one host' : undefined}>
                      {h}
                    </Chip>
                  );
                })}
              </div>
              <div style={{ display: 'flex', gap: 6, marginTop: 10, flexWrap: 'wrap' }}>
                <input data-testid="secret-host-new" style={{ ...fieldInput, ...mono, fontSize: 12, maxWidth: 240, padding: '5px 8px' }} value={newHost} placeholder="add a host, e.g. github.com"
                  autoFocus onChange={e => setNewHost(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addHost(); } if (e.key === 'Escape') setEditing(null); }} />
                <button type="button" data-testid="secret-host-add" style={btn(busy || newHost.trim() === '')}
                  disabled={busy || newHost.trim() === ''} onClick={addHost}>Add</button>
              </div>
              <div style={{ ...dim, fontSize: 11.5, marginTop: 8, lineHeight: 1.5 }}>
                The secret is only ever sent to these hosts. A host a linked repository still syncs from cannot be removed.
              </div>
              <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                <button type="button" data-testid="secret-hosts-save" style={btn(busy || !hostsChanged, 'primary')} disabled={busy || !hostsChanged}
                  onClick={() => run(() => api.patchSecret(secret.id, { hosts }), 'Hosts saved.')}>Save hosts</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setEditing(null)}>Cancel</button>
              </div>
            </div>
          )}
        </Prop>
        <Prop label="Expires" top={editing === 'expiry'}
          action={editing !== 'expiry' && <IconAction testid="secret-expiry-edit-start" icon={<PencilIcon color="currentColor" size={12} />} label="Change" onClick={() => begin('expiry')} />}>
          {editing !== 'expiry' ? (
            secret.expires_at ? (
              <span style={{ display: 'inline-flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
                {fmtDate(secret.expires_at)}
                <span style={{ ...dim, color: status.tone === 'bad' ? '#c98080' : status.tone === 'warn' ? '#c9a868' : '#777' }}>{relDays(secret.expires_at)}</span>
              </span>
            ) : <span style={{ color: '#999' }}>Never</span>
          ) : (
            <div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <input data-testid="secret-expiry-edit" type="date" autoFocus style={{ ...fieldInput, maxWidth: 170, padding: '5px 8px', colorScheme: 'dark' }} value={expiry}
                  onChange={e => setExpiry(e.target.value)} onKeyDown={e => { if (e.key === 'Escape') setEditing(null); }} />
                <button type="button" data-testid="secret-expiry-save" style={btn(busy || expiry === expiryDate(secret.expires_at), 'primary')}
                  disabled={busy || expiry === expiryDate(secret.expires_at)}
                  onClick={() => run(() => api.patchSecret(secret.id, { expires_at: expiryFromDate(expiry) }), expiry ? 'Expiry saved.' : 'Expiry cleared.')}>Save</button>
                {secret.expires_at && (
                  <button type="button" data-testid="secret-expiry-clear" style={btn(busy)} disabled={busy}
                    onClick={() => run(() => api.patchSecret(secret.id, { expires_at: null }), 'Expiry cleared.')}>No expiry</button>
                )}
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setEditing(null)}>Cancel</button>
              </div>
              <div style={{ ...dim, fontSize: 11.5, marginTop: 8 }}>A reminder only: sync keeps using the secret after this date until you replace it.</div>
            </div>
          )}
        </Prop>
      </Block>

      <Block title="Used by" aside={<span style={{ ...dim, fontSize: 11, fontVariantNumeric: 'tabular-nums' }}>{secret.linked_repos}</span>}>
        {!linked ? (
          <div style={{ ...dim, padding: '10px 0' }}>No repository uses this secret. Link one from the repository's Settings → Remote.</div>
        ) : (
          <div data-testid="secret-linked-repos" style={{ display: 'flex', flexDirection: 'column' }}>
            {(repos ?? []).map(r => (
              <div key={r} style={repoRow}>
                <RepoDot repo={r} />
                <span style={{ flex: 1, minWidth: 0, color: '#ddd' }}>{r}</span>
              </div>
            ))}
            {repos === null && <div style={{ ...dim, padding: '10px 0' }}>{secret.linked_repos} {repoWord(secret.linked_repos)}</div>}
          </div>
        )}
      </Block>

      <Block title="Delete" tone="danger" testid="secret-delete-block">
        <div style={{ display: 'flex', gap: 14, alignItems: 'center', padding: '10px 0', flexWrap: 'wrap' }}>
          <div style={{ flex: 1, minWidth: 220, ...dim, fontSize: 12.5, lineHeight: 1.55 }} data-testid={linked ? 'secret-delete-why' : undefined}>
            {linked
              ? <>Used by {secret.linked_repos} {repoWord(secret.linked_repos)}{repos && repos.length ? <>: <span style={{ color: '#bbb' }}>{repos.join(', ')}</span></> : null}. Unlink {secret.linked_repos === 1 ? 'it' : 'them'} first; then it can be deleted.</>
              : 'Removes the secret and its value for good. This cannot be undone.'}
          </div>
          {editing !== 'delete' ? (
            <button type="button" data-testid="secret-delete" style={{ ...btn(busy || linked, 'danger'), display: 'inline-flex', alignItems: 'center', gap: 6 }}
              disabled={busy || linked} title={linked ? 'Unlink every repository first' : undefined} onClick={() => begin('delete')}>
              <TrashIcon color="currentColor" size={12} /> Delete…
            </button>
          ) : (
            <span style={{ display: 'inline-flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              <span style={{ color: '#e8a0a0', fontSize: 12.5 }}>Delete {secret.name}?</span>
              <button type="button" data-testid="secret-delete-confirm" style={btn(busy, 'danger')} disabled={busy}
                onClick={async () => {
                  setBusy(true); setErr('');
                  try { await api.deleteSecret(secret.id); onDeleted(); } catch (e) { setErr(errMsg(e)); setBusy(false); setEditing(null); }
                }}>Delete</button>
              <button type="button" style={btn(busy)} disabled={busy} onClick={() => setEditing(null)}>Cancel</button>
            </span>
          )}
        </div>
      </Block>
    </div>
  );
}

const repoRow: CSSProperties = { display: 'flex', alignItems: 'center', gap: 10, padding: '8px 0', borderBottom: '1px solid #1a1a1a', fontSize: 13 };
