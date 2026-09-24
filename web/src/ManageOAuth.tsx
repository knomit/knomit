import { useCallback, useEffect, useState } from 'react';
import { api, type OAuthPending } from './api';
import { btn, card, cardLabel } from './manageStyles';

// ManageOAuth is the operator's side of an OAuth login (F19 phase 3b): a
// client such as Claude Code parked an authorization request on this
// instance's OAuth listener, and the operator approves or denies it here
// instead of with `knomit oauth approve` in a terminal. The tab exists only
// when the server has an OAuth issuer (the list call answers 404 otherwise,
// which RepoManager reads as "no tab").
//
// What a row shows is what the operator judges by, and where it comes from
// matters:
//  - client name and id, redirect URI, resource and requested scopes are
//    supplied by the REQUESTER. A URL client id proves only who owns a
//    domain, not which program is asking, so the redirect HOST is shown
//    first and largest: it is where the code will go.
//    (kb://bc6eac5f37df/kb/gotchas/ai/agents/tools/mcp/security/client-identity/066a9dad.md)
//  - remote address and user agent are those of the BROWSER that opened
//    the authorization link, not of the client program. Labelled so.
// Every one of them is rendered as text: React escapes it, and nothing here
// uses dangerouslySetInnerHTML.

// Requests expire after 10 minutes and someone is usually waiting in a
// browser, so this polls far faster than Sessions' 30 s.
const POLL_MS = 5_000;

// The scopes an approval may grant, in the server's canonical order
// (oauth.ScopesSupported; admin is never grantable to a token).
const GRANTABLE = ['read', 'write', 'push:own', 'merge:main', 'operator'];

// defaultScopes is the server's own default ceiling (3a ruling D13):
// requested ∩ {read, write}, else read. Clients ask for everything the server
// advertises — Claude Code does — so pre-checking "what was asked" would make
// the careless click the widest grant.
export function defaultScopes(requested: string[]): string[] {
  const d = ['read', 'write'].filter(s => requested.includes(s));
  return d.length > 0 ? d : ['read'];
}

function redirectHost(uri: string): string {
  try {
    return new URL(uri).host || uri;
  } catch {
    return uri;
  }
}

function timeLeft(iso: string, now: number): string {
  const s = Math.round((new Date(iso).getTime() - now) / 1000);
  if (s <= 0) return 'expired';
  if (s < 60) return `${s}s left`;
  return `${Math.round(s / 60)} min left`;
}

const mono: React.CSSProperties = { fontFamily: 'ui-monospace, monospace', fontSize: 12, wordBreak: 'break-all' };
const dim: React.CSSProperties = { color: '#888', fontSize: 12 };
const errText: React.CSSProperties = { color: '#f87171', fontSize: 12, marginTop: 6 };

function PendingRow({ p, onDone }: { p: OAuthPending; onDone: () => void }) {
  const [subject, setSubject] = useState('');
  const [scopes, setScopes] = useState<string[]>(() => defaultScopes(p.scopes));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const toggle = (s: string) =>
    setScopes(cur => (cur.includes(s) ? cur.filter(x => x !== s) : GRANTABLE.filter(x => x === s || cur.includes(x))));

  const act = async (f: () => Promise<void>) => {
    setBusy(true);
    setErr('');
    try {
      await f();
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const canApprove = !busy && subject.trim() !== '' && scopes.length > 0;
  return (
    <div data-testid="oauth-row" style={card}>
      <div style={cardLabel}>Code goes to</div>
      <div data-testid="oauth-redirect-host" style={{ fontSize: 16, fontWeight: 600, color: '#eee' }}>{redirectHost(p.redirect_uri)}</div>
      <div style={mono}>{p.redirect_uri}</div>

      <div style={{ ...cardLabel, marginTop: 10 }}>Client (as it describes itself)</div>
      <div style={{ color: '#ddd' }}>{p.client_name}</div>
      <div style={mono}>{p.client_id}</div>

      <div style={{ ...cardLabel, marginTop: 10 }}>For</div>
      <div style={mono}>{p.resource}</div>

      <div style={{ ...cardLabel, marginTop: 10 }}>Requested</div>
      <div data-testid="oauth-requested" style={mono}>{p.scopes.join(' ')}</div>

      <div style={{ ...cardLabel, marginTop: 10 }}>The browser that opened the link</div>
      <div style={dim}>
        <span style={mono}>{p.remote_addr}</span> · {p.user_agent} · {timeLeft(p.expires_at, Date.now())}
      </div>

      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center', marginTop: 12 }}>
        <label style={dim}>
          Subject{' '}
          <input
            value={subject}
            onChange={e => setSubject(e.target.value)}
            placeholder="e.g. laptop"
            style={{ background: '#000', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '4px 6px' }}
          />
        </label>
        {GRANTABLE.map(s => (
          <label key={s} style={dim}>
            <input type="checkbox" value={s} checked={scopes.includes(s)} onChange={() => toggle(s)} aria-label={s} /> {s}
          </label>
        ))}
      </div>
      <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
        <button type="button" style={btn(!canApprove, 'primary')} disabled={!canApprove}
          onClick={() => act(() => api.approveOAuthPending(p.id, subject.trim(), scopes))}>
          Approve
        </button>
        <button type="button" style={btn(busy, 'danger')} disabled={busy}
          onClick={() => act(() => api.denyOAuthPending(p.id))}>
          Deny
        </button>
      </div>
      {err && <div role="alert" style={errText}>{err}</div>}
    </div>
  );
}

export function ManageOAuth({ onCount }: { onCount?: (n: number) => void }) {
  const [rows, setRows] = useState<OAuthPending[] | null>(null);
  const [err, setErr] = useState('');

  const load = useCallback(() => {
    api.listOAuthPending()
      .then(list => {
        setErr('');
        setRows(list ?? []);
        onCount?.(list?.length ?? 0);
      })
      .catch(e => setErr(e instanceof Error ? e.message : String(e)));
  }, [onCount]);

  useEffect(() => {
    load();
    const t = setInterval(load, POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  return (
    <div style={{ padding: '4px 2px' }}>
      <div style={dim}>
        Clients waiting for permission to use this instance. Approving issues a token that acts as the subject you name,
        limited to the scopes you check.
      </div>
      {err && <div role="alert" style={errText}>{err}</div>}
      {rows !== null && rows.length === 0 && !err && (
        <div style={{ ...dim, marginTop: 14 }}>No authorization requests are waiting.</div>
      )}
      {rows?.map(p => <PendingRow key={p.id} p={p} onDone={load} />)}
    </div>
  );
}
