import type { CSSProperties } from 'react';
import { AuthPage } from './ManageOAuth';
import { SecretsTab } from './SecretsTab';
import { accessTabsFor, type AccessTab } from './facets';

// AccessPage is the Access server page: who and what may reach this instance
// and its remotes. Its tabs are Secrets (stored credentials repositories link
// to), always, and Authorizations (OAuth clients waiting for approval), only
// when the server has an OAuth issuer. A later Connected accounts tab joins
// them here.
//
// Which tab shows is App's: useOAuthAvailability must know whether
// Authorizations is on screen (that page polls for itself).

const TAB_LABEL: Record<AccessTab, string> = { secrets: 'Secrets', auth: 'Authorizations' };

export function AccessPage({ tab, onTab, oauthAvailable, authCount, onAuthCount }: {
  tab: AccessTab;
  onTab: (t: AccessTab) => void;
  oauthAvailable: boolean;
  /** Authorization requests waiting, badged on the tab when above zero. */
  authCount: number | null;
  onAuthCount: (n: number) => void;
}) {
  const tabs = accessTabsFor(oauthAvailable);
  const shown: AccessTab = tabs.includes(tab) ? tab : 'secrets';
  return (
    <div data-testid="access-page" style={page}>
      <div role="tablist" aria-label="Access" data-testid="access-tabs" style={strip}>
        {tabs.map(t => {
          const on = t === shown;
          return (
            <button key={t} type="button" role="tab" aria-selected={on} data-testid={`access-tab-${t}`}
              onClick={() => onTab(t)} style={tabStyle(on)}>
              {TAB_LABEL[t]}
              {t === 'auth' && authCount !== null && authCount > 0 && (
                <span data-testid="access-tab-auth-badge" aria-label={`${authCount} waiting`} style={badge}>{authCount}</span>
              )}
            </button>
          );
        })}
      </div>
      <div style={bodyStyle}>
        {shown === 'secrets' && <SecretsTab />}
        {shown === 'auth' && <AuthPage onCount={onAuthCount} />}
      </div>
    </div>
  );
}

const page: CSSProperties = { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const bodyStyle: CSSProperties = { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const strip: CSSProperties = {
  display: 'flex', gap: 2, padding: '0 16px', borderBottom: '1px solid #1e1e1e', flexShrink: 0,
};
const tabStyle = (on: boolean): CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', gap: 6,
  background: 'none', border: 'none', borderBottom: `2px solid ${on ? '#6ea8fe' : 'transparent'}`,
  color: on ? '#eee' : '#888', fontSize: 12.5, padding: '8px 10px 7px', cursor: 'pointer',
});
const badge: CSSProperties = {
  fontSize: 10, padding: '0 5px', borderRadius: 8, lineHeight: 1.6,
  background: '#1d2a22', color: '#4ade80', fontVariantNumeric: 'tabular-nums',
};
