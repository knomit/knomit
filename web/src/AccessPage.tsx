import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import { api, type SecretView } from './api';
import { accessTabsFor, type AccessTab } from './facets';
import { useFacetLayout } from './facetLayout';
import { FacetSplitter } from './FacetColumns';
import { KeyIcon, ShieldCheckIcon } from './icons';
import { DetailBack, SectionRail, SectionStrip, type SectionEntry } from './AccessParts';
import { ACCESS_MEDIUM_PX, ACCESS_NARROW_PX, LIST_MAX_W, LIST_MIN_W, detailBody, detailBodyNarrow, useWidth } from './accessUtil';
import { SecretCreate, SecretDetail, SecretsList, SecretsOverview } from './SecretsTab';
import { NEW_SECRET, errMsg } from './secrets';
import { AuthList, AuthOverview, GrantDetail, PendingDetail, RequestGone } from './ManageOAuth';
import { approvalNotice, grantKey, pendingKey, useAuthData, type AuthData } from './oauthAccess';

// AccessPage is the Access server page: who and what may reach this instance
// and its remotes. It is laid out like Browse — three columns, each picking
// what the next one shows:
//
//   sections        │ items                │ detail
//   (target rail's  │ (the Library's       │ (the fact view's
//    place & look)  │  rows & selection)   │  band, then blocks)
//
// Sections: Secrets (stored credentials repositories link to), always; and
// Authorizations (OAuth clients waiting for approval, and the ones approved),
// only when the server has an OAuth issuer. Connected accounts (GitHub
// sign-in) joins them as a third section: an AccessTab, a SectionEntry and a
// list/detail pair, nothing else.
//
// Which section shows is App's (useOAuthAvailability must know whether
// Authorizations is on screen: that section polls for itself). The item
// picked in each section is this page's, kept per section while the server
// pages are open, so switching sections and back returns to the same item.
// With nothing picked the detail is the section's overview.
//
// Widths: the section rail at the target rail's 236px; the list at the
// Library's width, capped at LIST_MAX_W; the detail the rest. Below
// ACCESS_MEDIUM_PX of page the rail shrinks to its glyphs; below
// ACCESS_NARROW_PX one column shows at a time (sections as a strip on top,
// then the list, or the detail with a way back).

const SECTION_LABEL: Record<AccessTab, string> = { secrets: 'Secrets', auth: 'Authorizations' };

export function AccessPage({ tab, onTab, oauthAvailable, authCount, onAuthCount }: {
  tab: AccessTab;
  onTab: (t: AccessTab) => void;
  oauthAvailable: boolean;
  /** Authorization requests waiting, badged on the section when above zero. */
  authCount: number | null;
  onAuthCount: (n: number | null) => void;
}) {
  const tabs = accessTabsFor(oauthAvailable);
  const shown: AccessTab = tabs.includes(tab) ? tab : 'secrets';

  const box = useRef<HTMLDivElement>(null);
  const width = useWidth(box);
  const narrow = width > 0 && width < ACCESS_NARROW_PX;
  const compact = width > 0 && width < ACCESS_MEDIUM_PX;
  const layout = useFacetLayout();
  const listW = Math.max(LIST_MIN_W, Math.min(LIST_MAX_W, layout.width));

  // Selection, per section. null: the overview.
  const [picked, setPicked] = useState<Record<AccessTab, string | null>>({ secrets: null, auth: null });
  const pick = useCallback((k: string | null) => setPicked(p => ({ ...p, [shown]: k })), [shown]);
  const sel = picked[shown];

  // Secrets: read here, not in the section, so the rail can count them and
  // flag one that needs attention.
  const [secrets, setSecrets] = useState<SecretView[] | null>(null);
  const [secretsErr, setSecretsErr] = useState('');
  const loadSecrets = useCallback(() => {
    api.listSecrets()
      .then(l => { setSecretsErr(''); setSecrets(l); })
      .catch(e => setSecretsErr(errMsg(e)));
  }, []);
  useEffect(() => { loadSecrets(); }, [loadSecrets]);
  const [adding, setAdding] = useState(false);

  const secretsAttention = secrets?.some(s => s.status === 'expired') ? 'bad'
    : secrets?.some(s => s.status === 'expiring') ? 'warn' : null;
  const entries: SectionEntry<AccessTab>[] = tabs.map(t => t === 'secrets'
    ? { key: t, label: SECTION_LABEL[t], icon: p => <KeyIcon {...p} />, count: secrets?.length ?? null,
      attention: secretsAttention, attentionTitle: secretsAttention === 'bad' ? 'A secret has expired' : 'A secret expires soon' }
    : { key: t, label: SECTION_LABEL[t], icon: p => <ShieldCheckIcon {...p} />, waiting: authCount ?? null });

  // Focus moves between the columns with ← and →.
  const listRef = useRef<HTMLDivElement>(null);
  const detailRef = useRef<HTMLDivElement>(null);
  const railRef = useRef<HTMLDivElement>(null);
  const focusList = () => {
    const el = listRef.current;
    (el?.querySelector<HTMLElement>('[data-item][aria-current]') ?? el?.querySelector<HTMLElement>('[data-item]'))?.focus();
  };
  const focusRail = () => railRef.current?.querySelector<HTMLElement>('[aria-current]')?.focus();
  const focusDetail = () => detailRef.current?.focus();

  // ─── The section's list and detail ───
  let list: ReactNode;
  let detail: ReactNode;
  if (shown === 'secrets') {
    const current = secrets?.find(s => s.id === sel) ?? null;
    const startAdd = () => { setAdding(true); pick(NEW_SECRET); };
    list = (
      <SecretsList list={secrets} err={secretsErr} selected={sel} onSelect={pick} focusOnly={narrow}
        adding={adding} onAdd={startAdd} onLeft={focusRail} onRight={focusDetail} />
    );
    detail = sel === NEW_SECRET && adding ? (
      <SecretCreate onCancel={() => { setAdding(false); pick(null); }}
        onCreated={v => { setAdding(false); loadSecrets(); pick(v.id); }} />
    ) : current ? (
      <SecretDetail key={current.id} secret={current} onChanged={loadSecrets}
        onDeleted={() => { pick(null); loadSecrets(); }} />
    ) : (
      <SecretsOverview list={secrets} onPick={pick} />
    );
  } else {
    list = <AuthSection.List selected={sel} onSelect={pick} focusOnly={narrow} onLeft={focusRail} onRight={focusDetail} />;
    detail = <AuthSection.Detail selected={sel} onSelect={pick} />;
  }

  // ONE tree at every width: the sections, the list and the detail keep their
  // places, and a width only restyles them (a narrow page hides the list while
  // an item is open). Swapping trees at a breakpoint would remount the columns
  // and drop what is being typed in them: a subject, a new secret, a hosts
  // edit, the list's search, a notice.
  const listHidden = narrow && sel !== null;
  return (
    <div ref={box} data-testid="access-page" style={{ ...page, flexDirection: narrow ? 'column' : 'row' }}>
      <AuthProvider enabled={shown === 'auth'} onAuthCount={onAuthCount}>
        <div ref={railRef} style={narrow ? { flexShrink: 0 } : { display: 'flex' }}>
          {narrow
            ? <SectionStrip entries={entries} active={shown} onPick={onTab} />
            : <SectionRail entries={entries} active={shown} onPick={onTab} compact={compact} onRight={focusList} />}
        </div>
        <div ref={listRef} data-testid="access-list"
          style={listHidden ? { display: 'none' } : narrow ? listNarrow : { ...listCol, width: listW }}>
          {list}
        </div>
        {!narrow && <FacetSplitter testId="access-splitter" />}
        <div ref={detailRef} tabIndex={-1} data-testid="access-detail" style={detailCol}
          onKeyDown={e => { if (e.key === 'Escape' && !(e.target as HTMLElement).closest('input,textarea,select')) focusList(); }}>
          <div style={narrow ? detailBodyNarrow : detailBody}>
            {listHidden && <DetailBack label={SECTION_LABEL[shown]} onClick={() => pick(null)} />}
            {detail}
          </div>
        </div>
      </AuthProvider>
    </div>
  );
}

// ─── Authorizations ──────────────────────────────────────────────────────────

/** A notice an approval, a denial or a revocation leaves, and WHERE it
 *  belongs: `at` is the list key it is about, null for the overview. It shows
 *  only there, and goes as soon as the selection moves elsewhere. */
interface AuthNotice { at: string | null; text: string; tone: 'ok' | 'warn' }

interface AuthState {
  data: AuthData;
  notice: AuthNotice | null;
  setNotice: (n: AuthNotice | null) => void;
}

// The Authorizations data is read once for both columns (the list and the
// detail show the same queue), and only while the section is on screen. The
// notice lives here too, above the columns, so nothing that remounts a
// column can drop it.
const AuthCtx = createContext<AuthState | null>(null);
function AuthProvider({ enabled, onAuthCount, children }: { enabled: boolean; onAuthCount: (n: number | null) => void; children: ReactNode }) {
  // Always the same element, so switching sections never remounts the
  // columns (and the focus a keyboard move put in the rail survives).
  const data = useAuthData(onAuthCount, enabled);
  const [notice, setNotice] = useState<AuthNotice | null>(null);
  const value = useMemo(() => (enabled ? { data, notice, setNotice } : null), [enabled, data, notice]);
  return <AuthCtx.Provider value={value}>{children}</AuthCtx.Provider>;
}

const AuthSection = {
  List({ selected, onSelect, focusOnly, onLeft, onRight }: {
    selected: string | null; onSelect: (k: string) => void; focusOnly: boolean; onLeft: () => void; onRight: () => void;
  }) {
    const st = useContext(AuthCtx);
    if (!st) return null;
    return <AuthList data={st.data} selected={selected} onSelect={onSelect} focusOnly={focusOnly} onLeft={onLeft} onRight={onRight} />;
  },
  Detail({ selected, onSelect }: { selected: string | null; onSelect: (k: string | null) => void }) {
    const st = useContext(AuthCtx);
    const notice = st?.notice ?? null;
    const setNotice = st?.setNotice;
    // A notice is about one place: picking anything else drops it.
    useEffect(() => {
      if (notice && notice.at !== selected) setNotice?.(null);
    }, [selected, notice, setNotice]);
    if (!st) return null;
    const { data } = st;
    const shownNotice = (at: string | null) => (notice && notice.at === at ? notice : null);
    const dismiss = () => st.setNotice(null);
    // Leave a notice on the overview, and go there.
    const toOverview = (text: string, tone: AuthNotice['tone'] = 'ok') => {
      st.setNotice({ at: null, text, tone });
      onSelect(null);
      data.reload();
    };
    const p = selected?.startsWith('p:') ? data.pending?.find(x => pendingKey(x.id) === selected) : undefined;
    const g = selected?.startsWith('g:') ? data.grants?.find(x => grantKey(x.id) === selected) : undefined;
    if (p) {
      return (
        <PendingDetail key={p.id} p={p}
          onDone={n => toOverview(n)}
          onApproved={(a, req) => {
            // Back to the overview: the login itself is listed under Approved
            // only once the client's browser collects the approval.
            const n = approvalNotice(a, req);
            toOverview(n.text, n.tone);
          }} />
      );
    }
    if (g) {
      return <GrantDetail key={g.id} g={g} notice={shownNotice(grantKey(g.id))} onDismissNotice={dismiss}
        onRevoked={n => toOverview(n)} />;
    }
    if (selected?.startsWith('p:') && data.pending !== null) return <RequestGone />;
    return <AuthOverview data={data} notice={shownNotice(null)} onDismissNotice={dismiss} onPick={onSelect} />;
  },
};

const page: CSSProperties = { flex: 1, minHeight: 0, minWidth: 0, display: 'flex', overflow: 'hidden' };
const listCol: CSSProperties = {
  flexShrink: 0, minHeight: 0, display: 'flex', flexDirection: 'column', background: '#121212',
};
// Narrow, with nothing open: the list on top, the overview under it.
const listNarrow: CSSProperties = {
  flexShrink: 0, maxHeight: '60%', minHeight: 0, display: 'flex', flexDirection: 'column', borderBottom: '1px solid #222', background: '#121212',
};
const detailCol: CSSProperties = { flex: 1, minWidth: 0, minHeight: 0, overflowY: 'auto', outline: 'none' };
