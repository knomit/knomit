import { useEffect, useState } from 'react';
import { api } from './api';
import type { Lens, OriginResponse, RepoInfo } from './api';
import { LENS, repoHue } from './utils';
import { btn, cardLabel } from './manageStyles';
import { PlusIcon, LayersIcon, RefreshIcon } from './icons';

// ManageOverview is the Manage mode's landing page.
//
// It carries NO statistics. Facts, commits, domains, entities, types,
// confidence, recency, highlights and the per-mount activity meter all already
// live in the browse summary (RightPanel / FacetPanel / RepoRows), and a second
// home for them would be two places to disagree about one number. What is left
// for Manage is configuration, so this page answers the two questions the rest
// of the app cannot: what needs doing, and what is wired to what.
//
// Deliberately NOT here: a "no licence" check. LICENSE is read-only — the Go
// side has ReadReadme/WriteReadme but only a read path for LicensePath — so a
// row offering to fix it would name an action that does not exist. It stays a
// column in the table, which reports, rather than a checklist row, which acts.

/** One repository's configuration, assembled from the per-repo fan-out. */
interface FleetRow {
  repo: string;
  agentBranch: string;
  license: string;
  origin: OriginResponse | null;
  /** The origin request FAILED — distinct from "no origin configured". */
  originError: boolean;
  loaded: boolean;
}

export interface Attention {
  repo: string;
  kind: 'push' | 'sync' | 'no-remote';
  title: string;
  detail: string;
}

/** remoteFailure reports the failing half of a remote, if either is failing. */
function remoteFailure(o: OriginResponse): { kind: 'push' | 'sync'; detail: string } | null {
  // The two halves fail INDEPENDENTLY — a fetch can recover while a push is
  // still rejected — so they are checked apart, exactly as the banner does.
  if (o.last_push_status && o.last_push_status !== 'ok') {
    return { kind: 'push', detail: o.last_push_error || 'the last push did not complete' };
  }
  if (o.last_status && o.last_status !== 'ok') {
    return { kind: 'sync', detail: o.last_error || 'the last sync did not complete' };
  }
  return null;
}

function attentionFor(rows: FleetRow[]): Attention[] {
  const out: Attention[] = [];
  for (const r of rows) {
    if (!r.loaded || r.originError) continue; // unknown is not the same as broken
    if (r.origin) {
      const fail = remoteFailure(r.origin);
      if (fail) {
        out.push({
          repo: r.repo,
          kind: fail.kind,
          title: fail.kind === 'push' ? 'push rejected' : 'sync failed',
          detail: fail.detail,
        });
      }
    } else {
      out.push({
        repo: r.repo,
        kind: 'no-remote',
        title: 'no remote configured',
        detail: 'This repository exists only on this machine.',
      });
    }
  }
  // Failures before absences: a broken remote is losing writes now, an
  // unconnected one merely never had any.
  return out.sort((a, b) => Number(a.kind === 'no-remote') - Number(b.kind === 'no-remote'));
}

export function ManageOverview({ repos, lenses, archivedCount, hideRemoteConfig, readOnly, onSelectRepo, onSelectLens, onNewRepo, onNewLens }: {
  repos: RepoInfo[];
  lenses: Lens[];
  archivedCount: number;
  hideRemoteConfig: boolean;
  /** Same gate as the rail's `+` buttons. Read-only is not only the
   *  server-read-only instance — it is also every history excursion — so these
   *  must be dead whenever those are, or Overview becomes the one way to reach
   *  a create form whose submit is going to be refused. */
  readOnly: boolean;
  /** focus names the block to scroll to on the repo's settings page. */
  onSelectRepo: (name: string, focus?: string) => void;
  onSelectLens: (name: string) => void;
  onNewRepo: () => void;
  onNewLens: () => void;
}) {
  // Only what has ARRIVED lives in state, keyed by repo. The rows the table
  // renders are derived below — seeding placeholders into state from the effect
  // body would be a synchronous setState and a cascading render, and the
  // placeholder is a pure function of `repos` anyway.
  const [loaded, setLoaded] = useState<Record<string, FleetRow>>({});

  // The fan-out. Every OTHER screen reads one repo's config — the active one —
  // which is why "which of my repositories is broken" has had no answer until
  // now. Two calls per repo, not three: the single-repo GET carries
  // agent_branch and license alongside the description.
  useEffect(() => {
    let cancelled = false;
    for (const { name: repo } of repos) {
      // Per repo, not per batch: one slow or failing repo must not hold up the
      // rest of the table, so each row fills itself in as its own pair lands.
      Promise.all([
        api.getRepo(repo).catch(() => null),
        hideRemoteConfig ? Promise.resolve(null) : api.getOrigin(repo).catch(() => 'error' as const),
      ]).then(([detail, origin]) => {
        if (cancelled) return;
        setLoaded(prev => ({
          ...prev,
          [repo]: {
            repo,
            agentBranch: detail?.agent_branch ?? '',
            license: detail?.license ?? '',
            origin: origin === 'error' ? null : origin,
            originError: origin === 'error',
            loaded: true,
          },
        }));
      });
    }
    return () => { cancelled = true; };
  }, [repos, hideRemoteConfig]);

  const rows: FleetRow[] = repos.map(r =>
    loaded[r.name] ?? { repo: r.name, agentBranch: '', license: '', origin: null, originError: false, loaded: false });

  const attention = hideRemoteConfig ? [] : attentionFor(rows);
  const lensesFor = (repo: string) => lenses
    .map(l => ({ name: l.name, write: l.write === repo, read: l.reads.some(r => r.repo === repo) }))
    .filter(m => m.write || m.read)
    .sort((a, b) => Number(b.write) - Number(a.write) || a.name.localeCompare(b.name));

  return (
    <div data-testid="manage-overview">
      <div style={head}>
        <div>
          <h3 style={{ margin: 0, fontSize: 17, fontWeight: 600 }}>Overview</h3>
          <div style={{ fontSize: 11.5, color: '#777', marginTop: 2 }}>
            {count(repos.length, 'repository', 'repositories')} · {count(lenses.length, 'lens', 'lenses')}
            {archivedCount > 0 && ` · ${archivedCount} archived`}
          </div>
        </div>
        {/* No Browse: Overview is not an entity, so there is nothing to browse.
            The header carries the two things you would come here to start.

            "+ Repository", not "+ New repository" — the plus already says new,
            and the word only made the button wider. */}
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          <button type="button" data-testid="overview-new-repo" style={btn(readOnly)} disabled={readOnly}
            title={readOnly ? 'Read-only' : undefined} onClick={onNewRepo}>
            <PlusIcon color="currentColor" size={12} /> Repository
          </button>
          <button type="button" data-testid="overview-new-lens" style={btn(readOnly)} disabled={readOnly}
            title={readOnly ? 'Read-only' : undefined} onClick={onNewLens}>
            <PlusIcon color="currentColor" size={12} /> Lens
          </button>
        </div>
      </div>

      {/* Attention first, and absent entirely when there is nothing to do. An
          empty "Needs attention: 0" heading would be a screen element whose
          only job is to say it has no job. */}
      {attention.length > 0 && (
        <div style={{ marginTop: 18 }}>
          <div style={cardLabel}>Needs attention · {attention.length}</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 7 }}>
            {attention.map(a => (
              <div key={`${a.repo}-${a.kind}`} data-testid={`attention-${a.repo}`} style={attRow(a.kind === 'no-remote')}>
                <span style={{ width: 8, height: 8, borderRadius: '50%', background: repoHue(a.repo), flexShrink: 0 }} />
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontSize: 12.5, color: '#eee' }}>
                    <b style={{ fontWeight: 600 }}>{a.repo}</b> — {a.title}
                  </div>
                  <div style={{ fontSize: 11, color: '#7a7a7a', marginTop: 1 }}>{a.detail}</div>
                </div>
                <div style={{ marginLeft: 'auto', flexShrink: 0 }}>
                  <button type="button" data-testid={`attention-fix-${a.repo}`} style={btn(false)}
                    onClick={() => onSelectRepo(a.repo, 'remote')}>
                    {/* Just "Connect": the row above it has already said "no
                        remote configured", so naming the object again is the
                        button repeating its own context back. The ellipsis is
                        kept — it opens a wizard rather than acting. */}
                    {a.kind === 'no-remote'
                      ? 'Connect…'
                      : <><RefreshIcon color="currentColor" size={11} /> Open remote</>}
                  </button>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      <div style={{ marginTop: 18 }}>
        <div style={cardLabel}>How things are wired</div>
        <table style={table}>
          <thead>
            <tr>
              <th style={{ ...th, width: '20%' }}>Repository</th>
              <th style={th}>Writes to</th>
              {!hideRemoteConfig && <th style={th}>Remote</th>}
              <th style={th}>Licence</th>
              <th style={{ ...th, width: '26%' }}>Mounted in</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(r => {
              const mounts = lensesFor(r.repo);
              const shown = mounts.slice(0, 2);
              return (
                <tr key={r.repo} data-testid={`fleet-row-${r.repo}`}>
                  {/* The repo name opens its settings page — same as picking it
                      in the rail, which also moves to match. The CELLS below go
                      further and land on the block they name. */}
                  <td style={td}>
                    <button type="button" className="k-bare" style={repoLink} data-testid={`fleet-open-${r.repo}`}
                      onClick={() => onSelectRepo(r.repo)}>
                      <span style={{ width: 7, height: 7, borderRadius: '50%', background: repoHue(r.repo), display: 'inline-block', marginRight: 8 }} />
                      {r.repo}
                    </button>
                  </td>
                  <td style={td}>
                    <CellButton testid={`fleet-branch-${r.repo}`} onClick={() => onSelectRepo(r.repo, 'agent-branch')}>
                      <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#8af' }}>
                        {r.agentBranch || (r.loaded ? '—' : '…')}
                      </span>
                    </CellButton>
                  </td>
                  {!hideRemoteConfig && (
                    <td style={td}>
                      <CellButton testid={`fleet-remote-${r.repo}`} onClick={() => onSelectRepo(r.repo, 'remote')}>
                        <RemotePill row={r} />
                      </CellButton>
                    </td>
                  )}
                  <td style={td}>
                    <CellButton testid={`fleet-license-${r.repo}`} onClick={() => onSelectRepo(r.repo, 'license')}>
                      {r.license
                        ? <span style={pillLic}>present</span>
                        : <span style={pillUnset}>{r.loaded ? 'none' : '…'}</span>}
                    </CellButton>
                  </td>
                  <td style={td}>
                    <span style={{ display: 'flex', alignItems: 'center', gap: 5, flexWrap: 'wrap' }}>
                      {shown.map(m => (
                        <button key={m.name} type="button" className="k-bare"
                          data-testid={`fleet-lens-${r.repo}-${m.name}`}
                          style={m.write ? pillLensWrite : pillLens}
                          onClick={() => onSelectLens(m.name)}>
                          <LayersIcon color="currentColor" size={9} />
                          {m.name}{m.write ? ' · write' : ''}
                        </button>
                      ))}
                      {/* Never a silent truncation: the count says what is not
                          shown, and opening the repo shows all of them. */}
                      {mounts.length > shown.length && (
                        <button type="button" className="k-bare" style={pillMore}
                          data-testid={`fleet-more-${r.repo}`}
                          title={mounts.map(m => m.name).join(', ')}
                          onClick={() => onSelectRepo(r.repo, 'mounted-in')}>
                          +{mounts.length - shown.length}
                        </button>
                      )}
                      {mounts.length === 0 && <span style={{ fontSize: 11, color: '#5a5a5a' }}>—</span>}
                    </span>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** RemotePill renders the four states a remote can be in, kept apart. */
function RemotePill({ row }: { row: FleetRow }) {
  if (!row.loaded) return <span style={pillUnset}>…</span>;
  // A failed READ is not "not connected": we do not know what is there, and
  // showing it as unconnected would invite connecting over a live remote.
  if (row.originError) return <span style={pillWarn}>unreadable</span>;
  if (!row.origin) return <span style={pillUnset}>local only</span>;
  const fail = remoteFailure(row.origin);
  if (fail) return <span style={pillBad}>{fail.kind === 'push' ? 'push rejected' : 'sync failed'}</span>;
  return <span style={pillOk}>in sync</span>;
}

function CellButton({ onClick, testid, children }: { onClick: () => void; testid: string; children: React.ReactNode }) {
  return (
    <button type="button" className="k-bare" style={cellBtn} data-testid={testid} onClick={onClick}>{children}</button>
  );
}

function count(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

// ── styles ──
const head: React.CSSProperties = { display: 'flex', alignItems: 'flex-start', gap: 12 };
const attRow = (soft: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 11,
  background: '#111', border: '1px solid #2a2a2a',
  borderLeft: `3px solid ${soft ? '#e2c07a' : '#f88'}`,
  borderRadius: 6, padding: '9px 12px',
});
const table: React.CSSProperties = { width: '100%', borderCollapse: 'collapse', fontSize: 12 };
const th: React.CSSProperties = {
  textAlign: 'left', fontSize: 9.5, letterSpacing: '0.13em', textTransform: 'uppercase',
  color: '#555', fontWeight: 500, padding: '0 10px 8px 0',
};
const td: React.CSSProperties = { padding: '7px 10px 7px 0', color: '#bbb', borderTop: '1px solid #1e1e1e', verticalAlign: 'middle' };
const repoLink: React.CSSProperties = {
  background: 'none', border: 'none', padding: 0, cursor: 'pointer',
  fontSize: 12.5, color: '#eee', textAlign: 'left',
};
const cellBtn: React.CSSProperties = { background: 'none', border: 'none', padding: 0, cursor: 'pointer', textAlign: 'left' };
const pillBase: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 5, fontSize: 10.5,
  padding: '2px 8px', borderRadius: 999, border: '1px solid #333', color: '#999',
  whiteSpace: 'nowrap', background: 'none', cursor: 'pointer',
};
const pillOk: React.CSSProperties = { ...pillBase, color: '#7c9', background: '#16241a', borderColor: '#2a4a2a' };
const pillBad: React.CSSProperties = { ...pillBase, color: '#f88', background: '#2a1717', borderColor: '#4a2a2a' };
const pillWarn: React.CSSProperties = { ...pillBase, color: '#e2c07a', background: '#262013', borderColor: '#4a3f22' };
const pillUnset: React.CSSProperties = { ...pillBase, color: '#7a7a7a', borderStyle: 'dashed' };
const pillLic: React.CSSProperties = { ...pillBase, color: '#c9b9f2', background: LENS.bg, borderColor: LENS.border };
const pillLens: React.CSSProperties = { ...pillBase, color: '#c9b9f2', borderColor: LENS.border, background: '#191728' };
const pillLensWrite: React.CSSProperties = { ...pillBase, color: '#b9dcc6', borderColor: '#4a6a52', background: '#16241a' };
const pillMore: React.CSSProperties = { ...pillBase, color: '#8a8a8a', borderStyle: 'dashed' };
