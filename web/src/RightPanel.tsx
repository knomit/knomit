import { useCallback, useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import { useAsync } from './hooks';
import { api } from './api';
import type { Fact, Stats, ActivityStats } from './api';
import type { AppState, Action } from './state';
import { currentPath, selectAnchorCommit, isReadOnly, READ_ONLY_TITLE } from './state';
import { relativeTime } from './utils';
import { RetractIcon } from './icons';
import { FactDiffView } from './FactDiffView';
import { FactBody, StatBox, TagCloud } from './FactBody';
import { VersionWalker } from './VersionWalker';

function renderFact(
  fact: Fact,
  repo: string,
  branch: string,
  dispatch: Dispatch<Action>,
  onRetract?: () => void,
  onScrub?: (commit: string, isLatest: boolean) => void,
  onHopRef?: (path: string, pinnedCommit: string) => void,
  readOnly = false,
  anchorCommit?: string | null,
) {
  const retractDisabled = readOnly;
  const retractTitle = retractDisabled ? READ_ONLY_TITLE : 'Retract fact';
  const retractColor = retractDisabled ? '#444' : '#f66';
  // Retracted-version badge: only when anchorCommit is set (history+history)
  // and fact.commit_hash is a different commit (the backend's ?fallback=before
  // walked back to a pre-retraction version). Compare 7-char prefixes since
  // anchorCommit may already be short.
  const anchorShort = anchorCommit ? anchorCommit.slice(0, 7) : '';
  const factShort = fact.commit_hash ? fact.commit_hash.slice(0, 7) : '';
  const retractedAt = anchorShort && factShort && anchorShort !== factShort ? anchorShort : '';
  // Pinned commit for in-body ref hops (narrowed to string for the closure).
  const refAnchor = fact.commit_hash;
  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div data-testid="fact-title" style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px', flex: 1, minWidth: 0 }}>
            {fact.title || fact.path}
          </div>
          <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0, marginTop: 4 }}>
            {fact.commit_date && (
              <span title={new Date(fact.commit_date).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
                {relativeTime(fact.commit_date)}
              </span>
            )}
            {fact.commit_hash && (
              <VersionWalker
                repo={repo}
                branch={branch}
                factPath={fact.path}
                currentCommit={fact.commit_hash}
                onScrub={onScrub ?? (() => {})}
              />
            )}
            {retractedAt && (
              <span
                data-testid="retracted-version-badge"
                title={`This fact was retracted at ${retractedAt}; showing its content from ${factShort}`}
                style={{ color: '#e5a23c', fontFamily: 'monospace', fontSize: 11, background: 'rgba(229,162,60,0.12)', border: '1px solid rgba(229,162,60,0.35)', padding: '1px 5px', borderRadius: 3 }}
              >
                retracted at {retractedAt}
              </span>
            )}
            {onRetract && (
              <button
                data-testid="retract-btn"
                title={retractTitle}
                disabled={retractDisabled}
                onClick={onRetract}
                style={{
                  background: 'none', border: 'none', padding: 2,
                  color: retractColor, cursor: retractDisabled ? 'not-allowed' : 'pointer',
                  display: 'flex', alignItems: 'center',
                  opacity: retractDisabled ? 0.4 : 0.6,
                }}
                onMouseEnter={e => { if (!retractDisabled) (e.currentTarget as HTMLElement).style.opacity = '1'; }}
                onMouseLeave={e => { if (!retractDisabled) (e.currentTarget as HTMLElement).style.opacity = '0.6'; }}
              ><RetractIcon color={retractColor} size={15} /></button>
            )}
          </span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 4 }}>
          <span style={{ fontSize: 12, color: '#555', fontFamily: 'monospace' }}>{fact.path}</span>
        </div>
      </div>

      <FactBody
        fact={fact}
        dispatch={dispatch}
        readOnly={readOnly}
        // Anchor the hop to THIS fact's own commit — the version of the edge
        // the referrer reasoned over — not the current viewing anchor. Reusing
        // the viewing anchor (repo HEAD when live) would make resolveHopAnchor
        // misclassify nearly every target as superseded and drop the UI into
        // read-only history mode. No commit_hash → no hop (matches old behavior).
        onRefClick={onHopRef && refAnchor ? (refPath: string) => onHopRef(refPath, refAnchor) : undefined}
      />
    </div>
  );
}

function FactEditor({ fact, repo, branch, readOnly, onSaved }: { fact: Fact; repo: string; branch: string; readOnly: boolean; onSaved: (updated: Fact) => void }) {
  const [raw, setRaw] = useState(fact.body);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const save = () => {
    if (readOnly) return;
    setSaving(true);
    setSaveError(null);
    api.updateFact(repo, branch, fact.path, raw)
      .then(updated => { setSaving(false); onSaved(updated); })
      .catch(e => { setSaving(false); setSaveError(String(e)); });
  };

  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box', display: 'flex', flexDirection: 'column', gap: 16 }}>
      <div style={{ background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.3)', borderRadius: 6, padding: '10px 14px' }}>
        <div style={{ color: '#f88', fontSize: 11, textTransform: 'uppercase', letterSpacing: 1.2, marginBottom: 4 }}>Parse error</div>
        <div style={{ color: '#f44', fontSize: 12, fontFamily: 'monospace' }}>{fact.parse_error}</div>
      </div>
      <div style={{ fontSize: 12, color: '#555' }}>{fact.path}</div>
      <textarea
        data-testid="fact-editor"
        value={raw}
        onChange={e => setRaw(e.target.value)}
        spellCheck={false}
        style={{
          flex: 1, minHeight: 320, background: '#0d0d14', color: '#ccc', border: '1px solid #2a2a3a',
          borderRadius: 6, padding: '12px 14px', fontFamily: 'monospace', fontSize: 12,
          lineHeight: 1.6, resize: 'none', outline: 'none', boxSizing: 'border-box', width: '100%',
        }}
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          data-testid="fact-save-btn"
          onClick={save}
          disabled={saving || readOnly}
          title={readOnly ? READ_ONLY_TITLE : undefined}
          style={{
            background: '#1a2e1a', border: '1px solid rgba(119,204,153,0.35)', color: '#7c9',
            padding: '6px 16px', borderRadius: 4,
            cursor: (saving || readOnly) ? 'not-allowed' : 'pointer',
            fontSize: 13, opacity: (saving || readOnly) ? 0.6 : 1,
          }}
        >{saving ? 'Saving\u2026' : 'Save'}</button>
        {saveError && <span style={{ color: '#f88', fontSize: 12 }}>{saveError}</span>}
      </div>
    </div>
  );
}

// ─── Confirm Modal ───────────────────────────────────────────────────────────

function ConfirmModal({ message, onConfirm, onCancel }: {
  message: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  // Close on Escape
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel();
      if (e.key === 'Enter') onConfirm();
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onConfirm, onCancel]);

  return (
    <div
      onClick={onCancel}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
        display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000,
      }}
    >
      <div
        onClick={e => e.stopPropagation()}
        style={{
          background: '#1a1a2a', border: '1px solid #333', borderRadius: 8,
          padding: '24px 28px', maxWidth: 400, width: '90%', boxShadow: '0 8px 32px rgba(0,0,0,0.6)',
        }}
      >
        <div style={{ fontSize: 13, color: '#ccc', lineHeight: 1.6, marginBottom: 20 }}>{message}</div>
        <div style={{ display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
          <button
            onClick={onCancel}
            style={{
              background: 'none', border: '1px solid #333', borderRadius: 4,
              color: '#888', cursor: 'pointer', padding: '6px 16px', fontSize: 12,
            }}
          >Cancel</button>
          <button
            data-testid="retract-confirm-btn"
            onClick={onConfirm}
            style={{
              background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.4)', borderRadius: 4,
              color: '#f66', cursor: 'pointer', padding: '6px 16px', fontSize: 12,
            }}
          >Retract</button>
        </div>
      </div>
    </div>
  );
}

// ─── Main RightPanel ─────────────────────────────────────────────────────────

export function RightPanel({ state, dispatch, onScrub, onHopRef }: {
  state: AppState;
  dispatch: Dispatch<Action>;
  onScrub?: (commit: string, isLatest: boolean) => void;
  onHopRef?: (path: string, pinnedCommit: string) => void;
}) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [stats, setStats] = useState<Stats | null>(null);
  const [activity, setActivity] = useState<ActivityStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [retracting, setRetracting] = useState(false);
  const [confirmRetract, setConfirmRetract] = useState(false);
  const path = currentPath(state);

  const factPath = state.factPath;
  const anchorCommit = selectAnchorCommit(state);
  const inDiff = state.asOf.mode === 'diff';

  // History asOf + anchor: opt into the backend's ?fallback=before so that
  // clicking a retracted file shows the pre-retraction content instead of a 404.
  const useFallback = state.asOf.mode === 'history' && !!anchorCommit;

  useAsync((stale) => {
    // In diff mode, FactDiffView owns the fact fetching via api.factDiff.
    // Skip this effect's fetch entirely so we don't issue a single-sided
    // request that gets discarded and may flash a 404 error.
    if (inDiff) { setFact(null); setError(null); return; }
    if (!factPath) { setFact(null); setError(null); return; }
    setError(null);
    setFact(null);
    api.fact(
      state.repo, state.branch, factPath,
      anchorCommit ?? undefined,
      useFallback ? { fallback: 'before' } : undefined,
    )
      .then(f => {
        if (stale()) return;
        setFact(f);
      })
      .catch(e => { if (!stale()) setError(String(e)); });
  }, [factPath, anchorCommit, state.repo, useFallback, inDiff]);

  useAsync((stale) => {
    if (factPath) return;
    Promise.all([
      api.stats(state.repo, state.branch, path).catch(() => null),
      api.activity(state.repo, state.branch, path).catch(() => null),
    ]).then(([s, a]) => {
      if (stale()) return;
      setStats(s);
      setActivity(a);
    });
  }, [factPath, state.repo, path, state.headCommit]);

  const doRetract = useCallback(() => {
    if (!fact || retracting || isReadOnly(state)) return;
    setConfirmRetract(false);
    setRetracting(true);
    api.retractFact(state.repo, state.branch, fact.path)
      .then(() => {
        setRetracting(false);
        // Clear the fact without touching headCommit. The git observer will
        // sync the index and then broadcast a status event with the new commit
        // hash, which triggers SET_HEAD in App.tsx. Only then will headCommit
        // change, ensuring the search/chrono re-fire against a fresh index.
        dispatch({ type: 'AMEND_NAV', factPath: null });
      })
      .catch(e => { setRetracting(false); setError(String(e)); });
  }, [fact, retracting, state, dispatch]);

  // Keyboard: ArrowLeft blurs right panel
  useEffect(() => {
    if (!state.rightPanelFocused) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'ArrowLeft') {
        e.preventDefault();
        dispatch({ type: 'BLUR_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, dispatch]);

  // Diff mode with a selected fact renders FactDiffView in the detail area.
  if (inDiff && state.factPath) {
    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <div style={{ flex: 1, overflow: 'auto', minHeight: 0 }}>
          <FactDiffView state={state as AppState & { factPath: string }} dispatch={dispatch} />
        </div>
      </div>
    );
  }

  if (error && factPath) {
    return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;
  }
  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  // Summary view: no fact selected
  if (!factPath) {
    const domainEntries = stats ? Object.entries(stats.domains).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const entityEntries = stats ? Object.entries(stats.entities).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const domainCount = stats ? Object.keys(stats.domains).length : 0;
    const entityCount = stats ? Object.keys(stats.entities).length : 0;
    const totalCommits = activity ? String(activity.total) : '\u2014';

    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <div data-testid="stats-view" style={{ flex: 1, padding: '24px 28px', overflowY: 'auto', boxSizing: 'border-box' }}>
          {stats ? (
            <>
              <div style={{ fontSize: 12, color: '#555', marginBottom: 20, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span>{stats.total} facts across {domainCount} domains</span>
                {activity?.last_commit && (
                  <span title={new Date(activity.last_commit).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
                    {relativeTime(activity.last_commit)}
                  </span>
                )}
              </div>
              <div style={{ display: 'flex', gap: 10, marginBottom: 28, flexWrap: 'wrap' }}>
                <StatBox label="Facts"      value={stats.total}                       color="#7c9" />
                <StatBox label="Confidence" value={stats.avg_confidence.toFixed(2)}   color="#8af" />
                <StatBox label="Domains"    value={domainCount}                        color="#fa8" />
                <StatBox label="Entities"   value={entityCount}                        color="#8af" />
                <StatBox label="Commits"    value={totalCommits}                       color="#555" />
              </div>
              <TagCloud label="Domains" entries={domainEntries} color="119,204,153"
                onTagClick={d => dispatch({ type: 'ADD_FILTER', chip: { category: 'domain', value: d } })} />
              <TagCloud label="Entities" entries={entityEntries} color="136,170,255"
                onTagClick={e => dispatch({ type: 'ADD_FILTER', chip: { category: 'entity', value: e } })} />
            </>
          ) : <div style={{ color: '#666' }}>No facts indexed in this path.</div>}
        </div>
      </div>
    );
  }

  // Fact view (normal or time-travel)
  if (!fact) return <div style={{ padding: 24, color: '#666' }}>Loading...</div>;

  if (fact.parse_error) return <FactEditor fact={fact} repo={state.repo} branch={state.branch} readOnly={isReadOnly(state)} onSaved={setFact} />;

  const readOnly = isReadOnly(state);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {confirmRetract && (
        <ConfirmModal
          message={`Are you sure you want to retract "${fact.title || fact.path}"?`}
          onConfirm={doRetract}
          onCancel={() => setConfirmRetract(false)}
        />
      )}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {renderFact(
          fact,
          state.repo,
          state.branch,
          dispatch,
          () => { if (!readOnly) setConfirmRetract(true); },
          onScrub,
          onHopRef,
          readOnly,
          // Only pass the anchor in history+history mode — the retracted-
          // version badge is only meaningful there. In live/diff/tree the
          // anchor either matches the fact's commit_hash (no badge) or is
          // null (badge suppressed).
          useFallback ? anchorCommit : null,
        )}
      </div>
    </div>
  );
}
