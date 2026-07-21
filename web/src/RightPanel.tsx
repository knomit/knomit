import { useCallback, useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import { useAsync } from './hooks';
import { api } from './api';
import type { Fact, Stats, ActivityStats, LensStats, LensRepoStats } from './api';
import type { AppState, Action } from './state';
import { currentPath, selectAnchorCommit, isReadOnly, READ_ONLY_TITLE, isLensContext, factHistoryAnchor, factTitleKey } from './state';
import { relativeTime, displayLensPath, repoHue, repoHueBg, repoHueBorder } from './utils';
import { RetractIcon, GitBranchIcon } from './icons';
import { FactDiffView } from './FactDiffView';
import { FactBody, StatBox, TagCloud } from './FactBody';
import { VersionWalker } from './VersionWalker';

// LensMeta prefixes a lens fact's breadcrumb with its source mount: a mono pill
// in the repo's deterministic hue (dot + repo name) and a blue branch chip.
// Mirrors the Library union-row badge (utils.repoHue*) so a fact reads the same
// wherever it appears. Repo-context facts render no LensMeta (lensMeta absent).
function LensMeta({ repo, branch }: { repo: string; branch: string }) {
  const c = repoHue(repo);
  return (
    <>
      <span
        data-testid="source-badge"
        data-repo={repo}
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 4, fontSize: 10,
          color: c, background: repoHueBg(repo), border: `1px solid ${repoHueBorder(repo)}`,
          borderRadius: 3, padding: '0 5px', fontFamily: 'var(--k-font-mono)', lineHeight: 1.6, flexShrink: 0,
        }}
      >
        <span style={{ width: 5, height: 5, borderRadius: '50%', background: c, flexShrink: 0 }} />
        {repo}
      </span>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 3, color: '#8af', flexShrink: 0 }}>
        <GitBranchIcon color="#8af" size={12} />
        <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11 }}>{branch}</span>
      </span>
    </>
  );
}

function renderFact(
  fact: Fact,
  // The fact's HISTORY anchor (factHistoryAnchor): its own source mount + the
  // RELATIVE path. VersionWalker reads versions through the mount's repo-scoped
  // /commits endpoint, so it must NOT use the write target or the raw kb:// path
  // (that pairing was the m36 no-op on read-mount facts).
  histAnchor: { repo: string; branch: string; path: string },
  dispatch: Dispatch<Action>,
  onRetract?: () => void,
  onScrub?: (commit: string) => void,
  onHopRef?: (path: string, pinnedCommit: string) => void,
  readOnly = false,
  anchorCommit?: string | null,
  lensMeta?: { repo: string; branch: string },
  readOnlyTitle: string = READ_ONLY_TITLE,
) {
  const retractDisabled = readOnly;
  const retractTitle = retractDisabled ? readOnlyTitle : 'Retract fact';
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
          <div data-testid="fact-title" style={{ fontFamily: 'var(--k-font-display)', fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px', flex: 1, minWidth: 0 }}>
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
                repo={histAnchor.repo}
                branch={histAnchor.branch}
                factPath={histAnchor.path}
                currentCommit={fact.commit_hash}
                onScrub={onScrub ?? (() => {})}
              />
            )}
            {retractedAt && (
              <span
                data-testid="retracted-version-badge"
                title={`This fact was retracted at ${retractedAt}; showing its content from ${factShort}`}
                style={{ color: '#e5a23c', fontFamily: 'var(--k-font-mono)', fontSize: 11, background: 'rgba(229,162,60,0.12)', border: '1px solid rgba(229,162,60,0.35)', padding: '1px 5px', borderRadius: 3 }}
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
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 4, flexWrap: 'wrap' }}>
          {lensMeta && <LensMeta repo={lensMeta.repo} branch={lensMeta.branch} />}
          <span style={{ fontSize: 12, color: '#555', fontFamily: 'var(--k-font-mono)' }}>
            {lensMeta ? displayLensPath(fact.path) : fact.path}
          </span>
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

function FactEditor({ fact, repo, branch, readOnly, onSaved, readOnlyTitle = READ_ONLY_TITLE }: { fact: Fact; repo: string; branch: string; readOnly: boolean; onSaved: (updated: Fact) => void; readOnlyTitle?: string }) {
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
        <div style={{ color: '#f44', fontSize: 12, fontFamily: 'var(--k-font-mono)' }}>{fact.parse_error}</div>
      </div>
      <div style={{ fontSize: 12, color: '#555' }}>{fact.path}</div>
      <textarea
        data-testid="fact-editor"
        value={raw}
        onChange={e => setRaw(e.target.value)}
        spellCheck={false}
        style={{
          flex: 1, minHeight: 320, background: '#0d0d14', color: '#ccc', border: '1px solid #2a2a3a',
          borderRadius: 6, padding: '12px 14px', fontFamily: 'var(--k-font-mono)', fontSize: 12,
          lineHeight: 1.6, resize: 'none', outline: 'none', boxSizing: 'border-box', width: '100%',
        }}
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          data-testid="fact-save-btn"
          onClick={save}
          disabled={saving || readOnly}
          title={readOnly ? readOnlyTitle : undefined}
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

// ─── Summary-view components ─────────────────────────────────────────────────

// StatsHistograms renders the top-10 domain + entity tag clouds. One
// implementation shared by the repo summary and the lens union header, so the
// two views cannot drift.
function StatsHistograms({ domains, entities, dispatch }: {
  domains: Record<string, number>;
  entities: Record<string, number>;
  dispatch: Dispatch<Action>;
}) {
  const domainEntries = Object.entries(domains).sort((a, b) => b[1] - a[1]).slice(0, 10);
  const entityEntries = Object.entries(entities).sort((a, b) => b[1] - a[1]).slice(0, 10);
  return (
    <>
      <TagCloud label="Domains" entries={domainEntries} color="119,204,153"
        onTagClick={d => dispatch({ type: 'ADD_FILTER', chip: { category: 'domain', value: d } })} />
      <TagCloud label="Entities" entries={entityEntries} color="136,170,255"
        onTagClick={e => dispatch({ type: 'ADD_FILTER', chip: { category: 'entity', value: e } })} />
    </>
  );
}

// LensRepoRow is one compact per-mount row under the union header: source
// badge in the repo's deterministic hue (matching Library union rows and
// LensMeta), a write marker on the lens's write repo, facts count, confidence,
// 1–2 top domains, and last-commit recency.
function LensRepoRow({ repo }: { repo: LensRepoStats }) {
  const c = repoHue(repo.name);
  const topDomains = Object.entries(repo.domains)
    .sort((a, b) => b[1] - a[1]).slice(0, 2).map(([d]) => d);
  return (
    <div data-testid="lens-repo-row" data-repo={repo.name}
      style={{
        display: 'flex', alignItems: 'center', gap: 8, padding: '8px 10px',
        background: '#15151f', border: '1px solid #22222f', borderRadius: 6,
        marginBottom: 8, flexWrap: 'wrap',
      }}>
      <span style={{
        display: 'inline-flex', alignItems: 'center', gap: 4, fontSize: 11,
        color: c, background: repoHueBg(repo.name), border: `1px solid ${repoHueBorder(repo.name)}`,
        borderRadius: 3, padding: '0 6px', fontFamily: 'var(--k-font-mono)', lineHeight: 1.8,
      }}>
        <span style={{ width: 5, height: 5, borderRadius: '50%', background: c }} />
        {repo.name}
      </span>
      {repo.is_write && (
        <span data-testid="write-marker" title="Lens write repo"
          style={{ fontSize: 10, color: '#7c9', border: '1px solid rgba(119,204,153,0.35)', borderRadius: 3, padding: '0 5px', lineHeight: 1.8 }}>
          write
        </span>
      )}
      <span style={{ fontSize: 11, color: '#999' }}>{repo.total} facts</span>
      <span style={{ fontSize: 11, color: '#8af' }}>conf {repo.avg_confidence.toFixed(2)}</span>
      {topDomains.length > 0 && (
        <span style={{ fontSize: 11, color: '#7c9' }}>{topDomains.join(' · ')}</span>
      )}
      <span style={{ marginLeft: 'auto', fontSize: 11, color: '#555' }}
        title={repo.last_commit ? new Date(repo.last_commit).toLocaleString() : undefined}>
        {repo.last_commit ? relativeTime(repo.last_commit) : '—'}
      </span>
    </div>
  );
}

// LensStatsView is the lens-context summary: a union roll-up header (exact
// sums, total-weighted confidence, max last_commit — computed server-side by
// GET /lenses/{lens}/stats) over the merged histograms, then one compact row
// per mount.
function LensStatsView({ stats, dispatch }: { stats: LensStats; dispatch: Dispatch<Action> }) {
  const domainCount = Object.keys(stats.domains).length;
  const entityCount = Object.keys(stats.entities).length;
  return (
    <>
      <div data-testid="lens-stats-header"
        style={{ fontSize: 12, color: '#555', marginBottom: 20, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <span>{stats.total} facts {'·'} {stats.repo_count} repos</span>
        {stats.last_commit && (
          <span title={new Date(stats.last_commit).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
            updated {relativeTime(stats.last_commit)}
          </span>
        )}
      </div>
      <div style={{ display: 'flex', gap: 10, marginBottom: 28, flexWrap: 'wrap' }}>
        <StatBox label="Facts"      value={stats.total}                     color="#7c9" />
        <StatBox label="Confidence" value={stats.avg_confidence.toFixed(2)} color="#8af" />
        <StatBox label="Domains"    value={domainCount}                     color="#fa8" />
        <StatBox label="Entities"   value={entityCount}                     color="#8af" />
        <StatBox label="Repos"      value={stats.repo_count}                color="#555" />
      </div>
      <StatsHistograms domains={stats.domains} entities={stats.entities} dispatch={dispatch} />
      <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', margin: '4px 0 10px' }}>Repos</div>
      {stats.repos.map(r => <LensRepoRow key={r.id || r.name} repo={r} />)}
    </>
  );
}

// ─── Main RightPanel ─────────────────────────────────────────────────────────

export function RightPanel({ state, dispatch, onScrub, onHopRef }: {
  state: AppState;
  dispatch: Dispatch<Action>;
  onScrub?: (commit: string) => void;
  onHopRef?: (path: string, pinnedCommit: string) => void;
}) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [stats, setStats] = useState<Stats | null>(null);
  const [activity, setActivity] = useState<ActivityStats | null>(null);
  const [lensStats, setLensStats] = useState<LensStats | null>(null);
  const [lensStatsError, setLensStatsError] = useState<boolean>(false);
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

  // In a lens context the open fact must be read THROUGH the lens: factPath is
  // the RAW canonical address (bare for the write repo, kb://<id12>/… for a read
  // mount), which the repo-scoped api.fact endpoint can't resolve. getLensFact
  // resolves it and returns the source mount, which RightPanel re-dispatches
  // (coherent with Library's row-click open) so a failed/racing open can't strand
  // a stale factSource on the new factPath (the m30 regression).
  const lensCtx = isLensContext(state);
  const lensName = state.lens?.name;
  const lensWrite = state.lens?.write;
  // Read-set fingerprint: an edit that adds/removes a mount keeps the lens NAME
  // but changes the reads, so the stats effect must re-fetch on it (a same-name
  // SET_LENS does not touch state.repo/headCommit).
  const lensReadSig = state.lens
    ? state.lens.reads.map(r => `${r.repo}@${r.branch ?? ''}`).join(',')
    : '';

  // Resolve the write repo's AGENT branch for lens writes. The open fact's
  // factSource.branch is the WRITE MOUNT's READ branch (WriteMountBranch) — which
  // Lens.normalize preserves when the write repo is pinned (e.g. core@main), a
  // NON-agent branch. Writes must land on the agent branch (the only branch the
  // repo write handlers accept as WritableBranch), so resolve it explicitly and
  // cache it; the read-mount branch stays purely a display concern (meta line).
  const [writeBranch, setWriteBranch] = useState<string | null>(null);
  useAsync((stale) => {
    if (!lensCtx || !lensWrite) { setWriteBranch(null); return; }
    api.getAgentBranch(lensWrite)
      .then(b => { if (!stale()) setWriteBranch(b); })
      .catch(() => { /* fall back to state.branch (already the write agent branch) */ });
  }, [lensCtx, lensWrite]);

  useAsync((stale) => {
    // In diff mode, FactDiffView owns the fact fetching via api.factDiff.
    // Skip this effect's fetch entirely so we don't issue a single-sided
    // request that gets discarded and may flash a 404 error.
    if (inDiff) { setFact(null); setError(null); return; }
    if (!factPath) { setFact(null); setError(null); return; }
    setError(null);
    setFact(null);
    if (lensCtx && lensName) {
      // Anchored lens read (C1): a scrub/diff entered from an open fact carries
      // an anchorCommit drawn from the fact's OWN mount timeline (VersionWalker),
      // and factSource is already set to that mount. getLensFact ignores the
      // anchor (always live), which would show the live body while the retracted-
      // badge/scrub UI thinks it's off-live. Read the anchored version through the
      // mount's repo-scoped commit endpoint instead — exactly as the repo-context
      // branch does — via factHistoryAnchor (mount repo/branch + RELATIVE path).
      // factSource is unchanged (same fact, same mount) so we don't re-dispatch it.
      if (anchorCommit && state.factSource) {
        const a = factHistoryAnchor(state);
        api.fact(
          a.repo, a.branch, a.path,
          anchorCommit,
          useFallback ? { fallback: 'before' } : undefined,
        )
          .then(f => { if (!stale()) setFact(f); })
          .catch(e => { if (!stale()) setError(String(e)); });
        return;
      }
      api.getLensFact(lensName, factPath)
        .then(f => {
          if (stale()) return;
          setFact(f);
          dispatch({ type: 'SET_FACT_SOURCE', source: f.source });
        })
        .catch(e => {
          if (stale()) return;
          setError(String(e));
          // m30: never leave a stale source paired with the new (failed) fact.
          dispatch({ type: 'SET_FACT_SOURCE', source: null });
        });
      return;
    }
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
  }, [factPath, anchorCommit, state.repo, useFallback, inDiff, lensCtx, lensName]);

  // Cache the loaded fact's title so the breadcrumb labels this crumb with the
  // title we already read — instead of a separate fetch that 404s for a
  // retracted fact. Keyed identically to the breadcrumb's crumb key.
  useEffect(() => {
    if (fact?.title && factPath) {
      dispatch({ type: 'CACHE_FACT_TITLE', key: factTitleKey(factPath, anchorCommit ?? undefined), title: fact.title });
    }
  }, [fact, factPath, anchorCommit, dispatch]);

  useAsync((stale) => {
    if (factPath) return;
    if (lensCtx) {
      // Lens context: ONE union stats call through the lens endpoint. The
      // repo-scoped stats/activity pair would describe the WRITE mount only —
      // silently misleading while browsing a union (design 2026-07-20). Clear
      // prior state so a lens switch never flashes the old lens's rows, and a
      // failed fetch (the backend fails the WHOLE request on any mount error —
      // RFC §9.1) surfaces as an error, NOT a false "no facts" empty state.
      setLensStats(null);
      setLensStatsError(false);
      // Lens named but not yet resolved (state.lens still stale/null): wait for
      // the resolution effect rather than fetching the wrong lens or falling
      // through to a pointless repo-scoped fetch.
      if (!lensName) return;
      api.getLensStats(lensName, path)
        .then(s => { if (!stale()) setLensStats(s); })
        .catch(() => { if (!stale()) setLensStatsError(true); });
      return;
    }
    Promise.all([
      api.stats(state.repo, state.branch, path).catch(() => null),
      api.activity(state.repo, state.branch, path).catch(() => null),
    ]).then(([s, a]) => {
      if (stale()) return;
      setStats(s);
      setActivity(a);
    });
  }, [factPath, state.repo, path, state.headCommit, lensCtx, lensName, lensReadSig]);

  // The repo-scoped write target for edits/retracts: {state.repo, state.branch} in
  // a repo context (unchanged); {lens.write, write-agent-branch} in a lens context.
  // The bare fact path is already write-repo-relative, so the existing
  // api.updateFact/retractFact reach the fact. Until getAgentBranch resolves we
  // fall back to state.branch, which in a lens context IS the write repo's agent
  // branch (App's status bootstrap resolved it the same way).
  const writeTarget = (lensCtx && lensWrite)
    ? { repo: lensWrite, branch: writeBranch ?? state.branch }
    : { repo: state.repo, branch: state.branch };
  // A lens fact is writable only when it lives in the lens's WRITE repo; read-mount
  // facts render fully read-only. Repo context keeps its prior gate (isReadOnly).
  const isWriteFact = !lensCtx || state.factSource?.repo === lensWrite;
  const factReadOnly = isReadOnly(state) || !isWriteFact;
  const factReadOnlyTitle = (!isWriteFact && lensWrite)
    ? `Read-only mount — edits go to ${lensWrite}`
    : READ_ONLY_TITLE;
  const lensMeta = lensCtx && state.factSource
    ? { repo: state.factSource.repo, branch: state.factSource.branch }
    : undefined;

  const doRetract = useCallback(() => {
    if (!fact || retracting || factReadOnly) return;
    setConfirmRetract(false);
    setRetracting(true);
    api.retractFact(writeTarget.repo, writeTarget.branch, fact.path)
      .then(() => {
        setRetracting(false);
        // Clear the fact without touching headCommit. The git observer will
        // sync the index and then broadcast a status event with the new commit
        // hash, which triggers SET_HEAD in App.tsx. Only then will headCommit
        // change, ensuring the search/chrono re-fire against a fresh index.
        dispatch({ type: 'AMEND_NAV', factPath: null });
      })
      .catch(e => { setRetracting(false); setError(String(e)); });
  }, [fact, retracting, factReadOnly, writeTarget.repo, writeTarget.branch, dispatch]);

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
    if (lensCtx) {
      return (
        <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
          <div data-testid="stats-view" style={{ flex: 1, padding: '24px 28px', overflowY: 'auto', boxSizing: 'border-box' }}>
            {lensStatsError
              ? <div data-testid="lens-stats-error" style={{ color: '#f88' }}>Couldn’t load lens stats — a mount failed to respond.</div>
              : lensStats
                ? <LensStatsView stats={lensStats} dispatch={dispatch} />
                : <div style={{ color: '#666' }}>Loading lens stats…</div>}
          </div>
        </div>
      );
    }
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
              <StatsHistograms domains={stats.domains} entities={stats.entities} dispatch={dispatch} />
            </>
          ) : <div style={{ color: '#666' }}>No facts indexed in this path.</div>}
        </div>
      </div>
    );
  }

  // Fact view (normal or time-travel)
  if (!fact) return <div style={{ padding: 24, color: '#666' }}>Loading...</div>;

  if (fact.parse_error) return <FactEditor fact={fact} repo={writeTarget.repo} branch={writeTarget.branch} readOnly={factReadOnly} readOnlyTitle={factReadOnlyTitle} onSaved={setFact} />;

  const readOnly = factReadOnly;

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
          // History anchor (VersionWalker) — the fact's own source mount + relative
          // path, NOT the write target. Repo context: {state.repo, state.branch, path}.
          factHistoryAnchor(state, fact.path),
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
          lensMeta,
          factReadOnlyTitle,
        )}
      </div>
    </div>
  );
}
