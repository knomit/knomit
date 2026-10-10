import { useCallback, useEffect, useState } from 'react';
import { api } from './api';
import { noMouseFocus } from './utils';
import { BotIcon, ScrollIcon, GlobeIcon } from './icons';
import { ManageFleet } from './ManageFleet';
import { ManageLogs } from './ManageLogs';
import { ManageOAuth } from './ManageOAuth';

interface Props {
  open: boolean;
  /**
   * The INSTANCE accepts no mutations (demo mode) — the only read-only this
   * pane takes from outside. Required so every render has to answer it.
   *
   * Deliberately NOT the app's isReadOnly(state): that folds in the BROWSED
   * repo's subscription, branch and history anchor, and nothing here is about
   * the browsed repo — the server's fleet membership, its log and its OAuth
   * requests are instance-scoped (#324).
   *
   * When per-caller permissions reach the UI, they belong here: the server's
   * counterpart is writeGate (internal/web/readonly.go).
   */
  serverReadOnly: boolean;
}

// The server's own pages. Everything that belongs to one repository or lens —
// its settings, its sessions, creating it, archiving it — moved into that
// target's facets and the rail; what is left behind the gear is the server:
// its fleet membership, its log and the OAuth requests waiting for the
// operator.
type Page = 'fleet' | 'logs' | 'oauth';

export function RepoManager({ open, serverReadOnly }: Props) {
  const [page, setPage] = useState<Page>('fleet');

  // The Authorizations tab (F19 phase 3b). The list call is the probe: null
  // (404) means this server has no OAuth issuer — [oauth] unset, or the
  // desktop, which never opens the OAuth listener — and then there is no tab.
  // A refusal (403: say, loopback-anonymous without admin) still shows the
  // tab, because the page is where the reason is explained. Not polled while
  // the page is open: it polls for itself and reports through onCount.
  const [oauthAvailable, setOAuthAvailable] = useState(false);
  const [oauthCount, setOAuthCount] = useState<number | null>(null);
  const oauthOpen = page === 'oauth';
  useEffect(() => {
    if (!open || serverReadOnly || oauthOpen) return;
    let cancelled = false;
    const probe = () => {
      api.listOAuthPending()
        .then(list => {
          if (cancelled) return;
          setOAuthAvailable(list !== null);
          setOAuthCount(list === null ? null : list.length);
        })
        .catch(() => { if (!cancelled) { setOAuthAvailable(true); setOAuthCount(null); } });
    };
    probe();
    const t = setInterval(probe, 30_000);
    return () => { cancelled = true; clearInterval(t); };
  }, [open, serverReadOnly, oauthOpen]);
  const handleOAuthCount = useCallback((n: number) => setOAuthCount(n), []);

  if (!open) return null;

  // Logs and Authorizations are absent under read-only, not disabled: their
  // endpoints answer 403 there, so a tab would be a control that cannot work.
  // A page whose tab is gone falls back to Fleet.
  const shown: Page = (page === 'logs' && serverReadOnly) || (page === 'oauth' && (serverReadOnly || !oauthAvailable))
    ? 'fleet' : page;

  return (
    <div style={surface} data-testid="manage-surface" aria-label="Manage">
      <div style={tabStrip} role="tablist" aria-label="Server pages">
        <button
          type="button"
          role="tab"
          data-testid="repomgr-fleet"
          aria-selected={shown === 'fleet'}
          onMouseDown={noMouseFocus}
          style={tabBtn(shown === 'fleet')}
          onClick={() => setPage('fleet')}
        >
          <BotIcon color="currentColor" size={12} /> Fleet
        </button>
        {!serverReadOnly && (
          <button
            type="button"
            role="tab"
            data-testid="repomgr-logs"
            aria-selected={shown === 'logs'}
            onMouseDown={noMouseFocus}
            style={tabBtn(shown === 'logs')}
            onClick={() => setPage('logs')}
          >
            <ScrollIcon color="currentColor" size={12} /> Logs
          </button>
        )}
        {!serverReadOnly && oauthAvailable && (
          <button
            type="button"
            role="tab"
            data-testid="repomgr-oauth"
            aria-selected={shown === 'oauth'}
            onMouseDown={noMouseFocus}
            style={tabBtn(shown === 'oauth')}
            onClick={() => setPage('oauth')}
          >
            <GlobeIcon color="currentColor" size={12} /> Authorizations
            {oauthCount !== null && oauthCount > 0 && (
              <span data-testid="repomgr-oauth-badge" style={tabBadge}>{oauthCount}</span>
            )}
          </button>
        )}
      </div>

      <div style={body}>
        <section data-testid="manage-detail" style={detailCol}>
          {shown === 'fleet' && <ManageFleet />}
          {shown === 'logs' && <ManageLogs />}
          {shown === 'oauth' && <ManageOAuth onCount={handleOAuthCount} />}
        </section>
      </div>
    </div>
  );
}

// ── styles ──
// surface is the whole Manage mode: it fills the window below the top bar
// rather than floating over it.
const surface: React.CSSProperties = {
  flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column',
  background: '#141414', color: '#eee',
};
const body: React.CSSProperties = { display: 'flex', flex: 1, minHeight: 0 };
const detailCol: React.CSSProperties = { flex: 1, padding: 20, overflowY: 'auto' };
const tabStrip: React.CSSProperties = {
  display: 'flex', gap: 2, padding: '8px 10px 0',
  borderBottom: '1px solid #222', flexShrink: 0,
};
const tabBtn = (active: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 7,
  background: active ? '#22303a' : 'transparent',
  color: active ? '#eee' : '#9a9a9a',
  border: 'none', borderRadius: '4px 4px 0 0',
  padding: '7px 12px', fontSize: 13, cursor: 'pointer',
  marginBottom: -1,
});
const tabBadge: React.CSSProperties = {
  fontSize: 10, padding: '0 5px', borderRadius: 8,
  background: '#1d2a22', color: '#4ade80', fontVariantNumeric: 'tabular-nums',
};
