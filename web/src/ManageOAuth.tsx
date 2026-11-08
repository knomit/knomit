import { useCallback, useMemo, useState, type CSSProperties } from 'react';
import { api, type OAuthApproval, type OAuthGrant, type OAuthPending } from './api';
import { defaultScopes, grantKey, pendingKey, redirectHost, type AuthData } from './oauthAccess';
import { btn, fieldInput, wizardErr } from './manageStyles';
import { ShieldCheckIcon, PlugIcon, ClockIcon } from './icons';
import { CopyButton } from './fleetParts';
import {
  Badge, Block, Chip, DetailHeader, ItemRow, ListEmpty, ListGroup, ListHeader, Note, Prop, Stats,
} from './AccessParts';
import { ago, attnRow, dim, fmtDate, left, listKeys, listScroll, mono, relDays, tabStop, useNow } from './accessUtil';
import { errMsg } from './secrets';

// Access → Authorizations is the operator's side of an OAuth login (F19 phase
// 3b): a client such as Claude Code parked an authorization request on this
// instance's OAuth listener, and the operator approves or denies it here
// instead of with `knomit oauth approve` in a terminal. The section exists
// only when the server has an OAuth issuer (the pending list answers 404
// otherwise, which useOAuthAvailability reads as "no section").
//
// The list has two groups: WAITING (parked requests, each with its deadline)
// and APPROVED (the logins approved before that can still be used, which can
// be revoked: GET /api/v1/oauth/grants). A login appears there once the
// browser has collected the approval, not at Approve. While the grants list
// answers null (no issuer) the group is not drawn.
//
// What a request shows is what the operator judges by, and where it comes
// from matters:
//  - client name and id, redirect URI, resource and requested scopes are
//    supplied by the REQUESTER. A URL client id proves only who owns a
//    domain, not which program is asking, so the redirect HOST is shown
//    first and largest: it is where the code will go.
//    (kb://bc6eac5f37df/kb/gotchas/ai/agents/tools/mcp/security/client-identity/066a9dad.md)
//  - remote address and user agent are those of the BROWSER that opened
//    the authorization link, not of the client program. Labelled so.
// Every one of them is rendered as text: React escapes it, and nothing here
// uses dangerouslySetInnerHTML.

// The scopes an approval may grant, in the server's canonical order
// (oauth.ScopesSupported; admin is never grantable to a token).
const GRANTABLE = ['read', 'write', 'push:own', 'merge:main', 'operator'];
const SCOPE_HINT: Record<string, string> = {
  read: 'read facts and history',
  write: 'write facts on agent branches',
  'push:own': 'push its own branches to the remote',
  'merge:main': 'merge into main',
  operator: 'run operator commands on this instance',
};

const AMBER = '#e2c07a';
const shortDate = (iso: string) => new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
const clock = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
const GREEN = '#8fd1a5';

// ─── List ────────────────────────────────────────────────────────────────────

export function AuthList({ data, selected, onSelect, focusOnly, onLeft, onRight }: {
  data: AuthData;
  selected: string | null;
  onSelect: (key: string) => void;
  /** One column at a time: arrows move the focus, Enter opens (listKeys). */
  focusOnly?: boolean;
  onLeft: () => void;
  onRight: () => void;
}) {
  const now = useNow(1000);
  const [q, setQ] = useState('');
  const match = useCallback((...xs: string[]) => {
    const t = q.trim().toLowerCase();
    return !t || xs.some(x => x.toLowerCase().includes(t));
  }, [q]);
  const pending = useMemo(() => (data.pending ?? []).filter(p => match(p.client_name, p.client_id, redirectHost(p.redirect_uri))), [data.pending, match]);
  const grants = useMemo(() => (data.grants ?? []).filter(g => match(g.client_name, g.client_id, g.subject)), [data.grants, match]);
  const ids = [...pending.map(p => pendingKey(p.id)), ...grants.map(g => grantKey(g.id))];
  const nothing = data.pending !== null && data.pending.length === 0 && (data.grants === null || data.grants.length === 0);
  const tab = tabStop(ids, selected);
  return (
    <>
      <ListHeader query={q} onQuery={setQ} placeholder="Find a client or subject" />
      <div data-testid="auth-list" role="list" style={listScroll} onKeyDown={listKeys(ids, selected, onSelect, { onLeft, onRight, focusOnly })}>
        {data.err && <div role="alert" style={{ ...wizardErr, margin: 12 }}>{data.err}</div>}
        {data.pending === null && !data.err && <div style={{ ...dim, padding: 16 }}>Loading…</div>}
        {nothing && (
          <ListEmpty testid="auth-empty" icon={<ShieldCheckIcon color="#666" size={18} />} title="No clients yet">
            When an MCP client such as Claude Code signs in to this instance, its request waits here for you to approve.
          </ListEmpty>
        )}
        {data.pending !== null && !nothing && (
          <ListGroup label="Waiting" count={pending.length} testid="auth-group-waiting">
            {pending.length === 0 && <div style={groupEmpty}>{q.trim() ? 'No match.' : 'Nothing waiting.'}</div>}
            {pending.map(p => {
              const k = pendingKey(p.id);
              const urgent = new Date(p.expires_at).getTime() - now < 3 * 60_000;
              return (
                <ItemRow key={k} id={k} selected={selected === k} tabbable={tab === k} onSelect={() => onSelect(k)} testid="oauth-row-item"
                  glyph={<ClockIcon color={selected === k ? AMBER : '#9a8a60'} />}
                  title={p.client_name || p.client_id}
                  meta={<>→ {redirectHost(p.redirect_uri)}</>}
                  badge={<Badge tone={urgent ? 'warn' : 'info'} title={`Expires at ${clock(p.expires_at)}`}>{left(p.expires_at, now)}</Badge>} />
              );
            })}
          </ListGroup>
        )}
        {data.grants !== null && !nothing && (
          <ListGroup label="Approved" count={grants.length} testid="auth-group-approved">
            {grants.length === 0 && <div style={groupEmpty}>{q.trim() ? 'No match.' : 'No client has been approved yet.'}</div>}
            {grants.map(g => {
              const k = grantKey(g.id);
              const lapsing = new Date(g.refresh_expires_at).getTime() - now < 7 * 86_400_000;
              return (
                <ItemRow key={k} id={k} selected={selected === k} tabbable={tab === k} onSelect={() => onSelect(k)} testid="oauth-grant-item"
                  glyph={<PlugIcon color={selected === k ? GREEN : '#6f8f78'} />}
                  title={g.client_name || g.client_id}
                  meta={<>as {g.subject} · {g.scopes.join(' ')} · since {shortDate(g.created_at)}</>}
                  badge={lapsing ? <Badge tone="warn" title={`Its sign-in lapses on ${fmtDate(g.refresh_expires_at)}`}>{relDays(g.refresh_expires_at, now).replace(/^in /, '')}</Badge> : undefined} />
              );
            })}
          </ListGroup>
        )}
      </div>
    </>
  );
}
const groupEmpty: CSSProperties = { ...dim, padding: '4px 12px 10px' };

// ─── Overview (nothing picked) ───────────────────────────────────────────────

/** A notice to show: an outcome, in the tone it deserves. */
export interface ShownNotice { text: string; tone: 'ok' | 'warn' }

export function AuthOverview({ data, notice, onDismissNotice, onPick }: {
  data: AuthData;
  notice: ShownNotice | null;
  onDismissNotice: () => void;
  onPick: (key: string) => void;
}) {
  const now = useNow(1000);
  const pending = data.pending ?? [];
  return (
    <div data-testid="auth-overview">
      <DetailHeader kindIcon={<ShieldCheckIcon color="#7c9" size={12} />} kind="oauth" kindColor="#7c9"
        title="Client sign-ins"
        sub="MCP clients ask to use this instance over OAuth. Approving one issues it a token that acts as the subject you name, limited to the scopes you check; revoking it signs the client out." />
      {notice && <Note tone={notice.tone} testid="oauth-notice" onDismiss={onDismissNotice}>{notice.text}</Note>}
      <div style={{ marginTop: 22 }}>
        <Stats items={[
          { n: pending.length, label: 'waiting', tone: 'ok' },
          ...(data.grants !== null ? [{ n: data.grants.length, label: 'approved' }] : []),
        ]} />
      </div>
      {pending.length > 0 && (
        <Block title="Waiting for you">
          {pending.map(p => (
            <button key={p.id} type="button" onClick={() => onPick(pendingKey(p.id))} style={attnRow}
              onMouseEnter={e => { e.currentTarget.style.background = '#191922'; }}
              onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}>
              <ClockIcon color={AMBER} size={13} />
              <span style={{ color: '#ddd', fontSize: 13, flexShrink: 0 }}>{p.client_name || p.client_id}</span>
              <span style={{ ...mono, fontSize: 11.5, color: '#888', flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>→ {redirectHost(p.redirect_uri)}</span>
              <Badge tone="info">{left(p.expires_at, now)}</Badge>
            </button>
          ))}
        </Block>
      )}
      <Block title="From a terminal">
        <div style={{ ...dim, padding: '10px 0', lineHeight: 1.6 }}>
          The same queue is at <span style={{ ...mono, color: '#bbb' }}>knomit oauth pending</span>; approve with{' '}
          <span style={{ ...mono, color: '#bbb' }}>knomit oauth approve &lt;id&gt; --as &lt;subject&gt;</span>.
        </div>
      </Block>
    </div>
  );
}

// ─── A waiting request ───────────────────────────────────────────────────────

export function PendingDetail({ p, onDone, onApproved }: {
  p: OAuthPending;
  /** Denied: back to the overview. */
  onDone: (notice: string) => void;
  onApproved: (a: OAuthApproval, p: OAuthPending) => void;
}) {
  const now = useNow(1000);
  const [subject, setSubject] = useState('');
  const [scopes, setScopes] = useState<string[]>(() => defaultScopes(p.scopes));
  const [busy, setBusy] = useState(false);
  const [denying, setDenying] = useState(false);
  const [err, setErr] = useState('');

  const toggle = (s: string) =>
    setScopes(cur => (cur.includes(s) ? cur.filter(x => x !== s) : GRANTABLE.filter(x => x === s || cur.includes(x))));
  const act = async (f: () => Promise<void>) => {
    setBusy(true);
    setErr('');
    try {
      await f();
    } catch (e) {
      setErr(errMsg(e));
      setBusy(false);
    }
  };

  const expired = new Date(p.expires_at).getTime() <= now;
  const urgent = new Date(p.expires_at).getTime() - now < 3 * 60_000;
  const canApprove = !busy && !expired && subject.trim() !== '' && scopes.length > 0;
  const host = redirectHost(p.redirect_uri);
  const loopback = /^(localhost|127\.0\.0\.1|\[::1\])(:\d+)?$/.test(host);
  const custom = !/^https?:/i.test(p.redirect_uri);
  return (
    <div data-testid="oauth-row">
      <DetailHeader kindIcon={<ClockIcon color={AMBER} size={12} />} kind="waiting request" kindColor={AMBER}
        status={<Badge tone={expired ? 'bad' : urgent ? 'warn' : 'info'} dot title={`Expires at ${clock(p.expires_at)}`}>{expired ? 'expired' : left(p.expires_at, now)}</Badge>}
        aside={<>asked {ago(p.created_at, now)}</>}
        title={p.client_name || p.client_id}
        sub="Asks to sign in to this instance. Check where the sign-in code goes before you approve." />

      <div style={codeGoes}>
        <div style={{ fontSize: 10.5, letterSpacing: '0.1em', textTransform: 'uppercase', color: '#8a7a50', fontWeight: 600 }}>The sign-in code goes to</div>
        <div data-testid="oauth-redirect-host" style={{ ...mono, fontSize: 17, fontWeight: 600, color: '#f1e6cc', marginTop: 6, overflowWrap: 'anywhere' }}>{host}</div>
        <div style={{ ...mono, fontSize: 11.5, color: '#9a8f78', marginTop: 3, overflowWrap: 'anywhere' }}>{p.redirect_uri}</div>
        <div style={{ fontSize: 12, color: '#b0a281', marginTop: 8, lineHeight: 1.5 }}>
          {loopback ? 'A program on the machine that opened the link (loopback).'
            : custom ? 'An app registered for this link scheme on that machine.'
              : 'A web address. Approve only if you recognise it.'}
        </div>
      </div>

      <Block title="The client, as it describes itself">
        <Prop label="Name">{p.client_name || <span style={dim}>none given</span>}</Prop>
        <Prop label="Client id">
          <span style={{ ...mono, fontSize: 12, color: '#bbb', overflowWrap: 'anywhere' }}>{p.client_id}</span>
        </Prop>
        <Prop label="For">
          <span style={{ ...mono, fontSize: 12, color: '#bbb', overflowWrap: 'anywhere' }}>{p.resource}</span>
        </Prop>
        <Prop label="Asks for" top>
          <span data-testid="oauth-requested" style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {p.scopes.map(s => <Chip key={s}>{s}</Chip>)}
          </span>
        </Prop>
      </Block>

      <Block title="The browser that opened the link">
        <Prop label="Address"><span style={{ ...mono, fontSize: 12 }}>{p.remote_addr}</span></Prop>
        <Prop label="User agent">
          {/* <bdi>: a U+202E in the user agent must not reorder its neighbours (3b review N1). */}
          <bdi data-testid="oauth-user-agent" style={{ fontSize: 12.5, color: '#bbb', overflowWrap: 'anywhere' }}>{p.user_agent}</bdi>
        </Prop>
        <Prop label="Asked at">{clock(p.created_at)} <span style={dim}>· expires at {clock(p.expires_at)}</span></Prop>
      </Block>

      <Block title="Decide">
        <Prop label="Acts as" top>
          <div>
            <input value={subject} onChange={e => setSubject(e.target.value)} placeholder="e.g. laptop" aria-label="Subject"
              data-testid="oauth-subject"
              style={{ ...fieldInput, maxWidth: 260, padding: '5px 8px' }} />
            <div style={{ ...dim, fontSize: 11.5, marginTop: 6 }}>
              The token signs in as <span style={{ ...mono, color: '#bbb' }}>host:{subject.trim() || '<subject>'}@token</span>. Reuse a subject to share its grants.
            </div>
          </div>
        </Prop>
        <Prop label="May" top>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
            {GRANTABLE.map(s => {
              const on = scopes.includes(s);
              const asked = p.scopes.includes(s);
              return (
                <label key={s} style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '3px 0', cursor: 'pointer', fontSize: 12.5 }}>
                  <input type="checkbox" value={s} checked={on} onChange={() => toggle(s)} aria-label={s} style={{ accentColor: '#4a7ab5', margin: 0 }} />
                  <span style={{ ...mono, fontSize: 12, color: on ? '#eee' : '#999', minWidth: 84 }}>{s}</span>
                  <span style={{ color: '#777', fontSize: 12 }}>{SCOPE_HINT[s]}</span>
                  {!asked && <span style={{ color: '#555', fontSize: 11 }}>· not asked</span>}
                </label>
              );
            })}
          </div>
        </Prop>
        <div style={{ display: 'flex', gap: 8, marginTop: 16, alignItems: 'center', flexWrap: 'wrap' }}>
          <button type="button" data-testid="oauth-approve" style={btn(!canApprove, 'primary')} disabled={!canApprove}
            title={subject.trim() === '' ? 'Name the subject first' : undefined}
            onClick={() => act(async () => onApproved(await api.approveOAuthPending(p.id, subject.trim(), scopes), p))}>
            Approve
          </button>
          {!denying ? (
            <button type="button" data-testid="oauth-deny" style={btn(busy, 'secondary')} disabled={busy} onClick={() => setDenying(true)}>Deny…</button>
          ) : (
            <>
              <span style={{ color: '#e8a0a0', fontSize: 12.5 }}>Deny {p.client_name || 'this client'}?</span>
              <button type="button" data-testid="oauth-deny-confirm" style={btn(busy, 'danger')} disabled={busy}
                onClick={() => act(async () => { await api.denyOAuthPending(p.id); onDone(`Denied ${p.client_name || p.client_id}. Its browser is told the request was refused.`); })}>Deny</button>
              <button type="button" style={btn(busy)} disabled={busy} onClick={() => setDenying(false)}>Cancel</button>
            </>
          )}
        </div>
        {err && <div role="alert" style={wizardErr}>{err}</div>}
      </Block>
    </div>
  );
}
const codeGoes: CSSProperties = {
  marginTop: 18, padding: '12px 14px', borderRadius: 6,
  background: '#1c1912', border: '1px solid #3a321f',
};

/** RequestGone: the request picked is no longer in the queue. */
export function RequestGone() {
  return (
    <div data-testid="oauth-gone">
      <DetailHeader kindIcon={<ClockIcon color="#777" size={12} />} kind="request" kindColor="#888"
        title="No longer waiting"
        sub="This request was decided elsewhere (another tab, or knomit oauth in a terminal), or it expired. The client can ask again." />
    </div>
  );
}

// ─── An approved client ──────────────────────────────────────────────────────

export function GrantDetail({ g, notice, onDismissNotice, onRevoked }: {
  g: OAuthGrant;
  notice: ShownNotice | null;
  onDismissNotice: () => void;
  onRevoked: (notice: string) => void;
}) {
  const now = useNow(30_000);
  const [revoking, setRevoking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const principal = `host:${g.subject}@token`;
  const lapsing = new Date(g.refresh_expires_at).getTime() - now < 7 * 86_400_000;
  return (
    <div data-testid="oauth-grant">
      <DetailHeader kindIcon={<PlugIcon color={GREEN} size={12} />} kind="approved client" kindColor={GREEN}
        status={lapsing
          ? <Badge tone="warn" dot title="Its sign-in lapses then, and the client must ask again">sign-in lapses {relDays(g.refresh_expires_at, now)}</Badge>
          : <Badge tone="ok" dot>signed in</Badge>}
        aside={<>approved {relDays(g.created_at, now)}</>}
        title={g.client_name || g.client_id}
        sub={<>acts as <span style={{ ...mono, color: '#bbb' }}>{principal}</span></>} />
      {notice && <Note tone={notice.tone} testid="oauth-notice" onDismiss={onDismissNotice}>{notice.text}</Note>}

      <Block title="What it may do">
        <Prop label="Subject">
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            <span style={{ ...mono, fontSize: 12.5 }}>{g.subject}</span>
            <CopyButton text={principal} label="Copy the principal" />
          </span>
        </Prop>
        <Prop label="Scopes" top>
          <span style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>{g.scopes.map(s => <Chip key={s}>{s}</Chip>)}</span>
          <div style={{ ...dim, fontSize: 11.5, marginTop: 6 }}>
            A ceiling: the subject's own grants can cap it lower (<span style={mono}>knomit grants list {principal}</span>).
          </div>
        </Prop>
        <Prop label="For"><span style={{ ...mono, fontSize: 12, color: '#bbb', overflowWrap: 'anywhere' }}>{g.resource}</span></Prop>
      </Block>

      <Block title="History">
        <Prop label="Approved">{fmtDate(g.created_at)} <span style={dim}>· by <span style={mono}>{g.approved_by || 'unknown'}</span></span></Prop>
        {/* The server derives it from the newest token the login was issued
            (the code exchange, then each refresh), so it is when the client
            last got a token, not its last request. */}
        <Prop label="Last used">
          {g.last_used_at
            ? <span title="When it was last issued a token">{ago(g.last_used_at, now)}</span>
            : <span style={dim}>not yet</span>}
        </Prop>
        <Prop label="Sign-in lapses">
          {fmtDate(g.refresh_expires_at)} <span style={{ ...dim, color: lapsing ? '#c9a868' : '#777' }}>· {relDays(g.refresh_expires_at, now)}, then the client must ask again</span>
        </Prop>
        <Prop label="Client id">
          <span style={{ ...mono, fontSize: 12, color: '#bbb', overflowWrap: 'anywhere' }}>{g.client_id}</span>
        </Prop>
      </Block>

      <Block title="Revoke" tone="danger">
        <div style={{ display: 'flex', gap: 14, alignItems: 'center', padding: '10px 0', flexWrap: 'wrap' }}>
          <div style={{ flex: 1, minWidth: 220, ...dim, fontSize: 12.5, lineHeight: 1.55 }}>
            Signs {g.client_name || 'the client'} out: its access and refresh tokens stop working at once. It can ask again, and that request waits here.
          </div>
          {!revoking ? (
            <button type="button" data-testid="oauth-revoke" style={btn(busy, 'danger')} disabled={busy} onClick={() => setRevoking(true)}>Revoke…</button>
          ) : (
            <span style={{ display: 'inline-flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              <span style={{ color: '#e8a0a0', fontSize: 12.5 }}>Sign it out?</span>
              <button type="button" data-testid="oauth-revoke-confirm" style={btn(busy, 'danger')} disabled={busy}
                onClick={async () => {
                  setBusy(true); setErr('');
                  try { await api.revokeOAuthGrant(g.id); onRevoked(`Revoked ${g.client_name || g.client_id} (${principal}).`); }
                  catch (e) { setErr(errMsg(e)); setBusy(false); setRevoking(false); }
                }}>Revoke</button>
              <button type="button" style={btn(busy)} disabled={busy} onClick={() => setRevoking(false)}>Cancel</button>
            </span>
          )}
        </div>
        {err && <div role="alert" style={wizardErr}>{err}</div>}
      </Block>
    </div>
  );
}
