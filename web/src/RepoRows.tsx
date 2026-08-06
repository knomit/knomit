import type { Dispatch, CSSProperties } from 'react';
import type { Action } from './state';
import type { LensRepoStats } from './api';
import { relativeTime, repoHue } from './utils';

// RepoRows is the lens summary's per-mount list: one ranked row per mount under
// the facet columns.
//
// It replaces a stack of bordered cards (~51px each, ~330px for six mounts).
// The cards were the only boxed thing left in the panel after the facet clouds
// became ranked columns, and they encoded nothing: a 1377-fact mount and a
// 9-fact one carried the same visual weight. Rows here are built from the same
// three parts a FacetRow is — name, hairline share bar, tabular count — so the
// section reads as continuous with the block above it rather than imported.
//
// The activity meter is the one thing this list has that the facet columns
// don't. Every mount's changes_7d/30d/90d already ship in the stats payload and
// nothing rendered them; three ticks turn the list from an inventory into a
// pulse. Three ticks and NOT a sparkline is deliberate — the payload carries
// three cumulative buckets, not a time series, and a curve through them would
// be drawing data the server never sent.

// Count descending, then name ascending. The tie-break is the same rule (and
// the same reason) as FacetPanel.ranked: without it, JSON object order decides
// which of two equal-count mounts leads, and two renders of one corpus are free
// to disagree.
function ranked(repos: LensRepoStats[]): LensRepoStats[] {
  return [...repos].sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
}

// Columns: mount | facts | confidence | activity | recency. Fixed widths on
// everything but the name, so the numbers form columns the eye can read down.
const GRID = 'minmax(0, 1fr) 76px 52px 46px 64px';
const NAME_CELL_MAX = 380;

const labelStyle: CSSProperties = {
  fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555f6d',
};
const headCell: CSSProperties = {
  fontSize: 9, textTransform: 'uppercase', letterSpacing: '.11em', color: '#4b5361',
  paddingBottom: 6,
};
const numCell: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 10.5, textAlign: 'right',
  fontVariantNumeric: 'tabular-nums', paddingBottom: 5,
};

// Ticks are square-rooted, not linear: the write repo out-commits a read mount
// by an order of magnitude in a normal lens, and a linear scale would flatten
// every other mount to a single pixel.
const TICK_MAX = 12;
function tickHeight(v: number, max: number): number {
  if (v <= 0 || max <= 0) return 1.5;
  return Math.max(1.5, Math.sqrt(v / max) * TICK_MAX);
}

function RepoRow({ repo, maxTotal, maxChanges, onPick }: {
  repo: LensRepoStats; maxTotal: number; maxChanges: number; onPick: (name: string) => void;
}) {
  const hue = repoHue(repo.name);
  const empty = repo.total === 0;
  const topDomain = Object.entries(repo.domains)
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))[0]?.[0];
  const buckets: [number, string][] = [
    [repo.changes_7d, '7d'], [repo.changes_30d, '30d'], [repo.changes_90d, '90d'],
  ];
  return (
    <div data-testid="lens-repo-row" data-repo={repo.name} data-empty={empty ? 'true' : undefined}
      onClick={() => onPick(repo.name)}
      title={`Filter by ${repo.name}`}
      onMouseEnter={e => { e.currentTarget.style.background = '#14141c'; }}
      onMouseLeave={e => { e.currentTarget.style.background = 'none'; }}
      style={{
        display: 'grid', gridTemplateColumns: GRID, gap: 14, alignItems: 'center',
        padding: '4px 6px', margin: '0 -6px', borderRadius: 4, cursor: 'pointer',
        background: 'none', transition: 'background 0.12s',
        opacity: empty ? 0.55 : 1,
      }}>
      {/* Name cell. The share bar sits under the whole cell rather than under
          the name alone, so it reads as one measure per row — and the cell is
          capped well short of the 1fr track it sits in, because at full panel
          width the leading mount's bar stops reading as a measure and starts
          reading as a horizontal rule. The cap also keeps it at roughly the
          length of a facet-column bar, which is the scale the eye has just
          been calibrated on. */}
      <span style={{
        position: 'relative', display: 'flex', alignItems: 'center', gap: 7,
        minWidth: 0, maxWidth: NAME_CELL_MAX, paddingBottom: 5,
      }}>
        <span style={{ width: 5, height: 5, borderRadius: '50%', background: hue, flex: 'none' }} />
        <span style={{
          fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#b9c1cd',
          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        }}>{repo.name}</span>
        {repo.is_write && (
          <span data-testid="write-marker" title="Lens write repo"
            style={{
              fontSize: 9.5, color: '#7c9', border: '1px solid rgba(119,204,153,0.35)',
              borderRadius: 3, padding: '0 5px', lineHeight: 1.7, flex: 'none',
              fontFamily: 'var(--k-font-mono)',
            }}>write</span>
        )}
        {topDomain && (
          <span style={{
            fontSize: 10.5, color: '#6f9c81', opacity: 0.8, minWidth: 0,
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
          }}>{topDomain}</span>
        )}
        <span aria-hidden data-testid="repo-share" style={{
          position: 'absolute', left: 0, bottom: 0, height: 1.5, borderRadius: 1,
          width: `${maxTotal > 0 ? (repo.total / maxTotal) * 100 : 0}%`,
          background: hue, opacity: 0.6,
        }} />
      </span>

      <span style={{ ...numCell, color: '#8e99ab' }}>
        {repo.total} <span style={{ color: '#5a6675' }}>facts</span>
      </span>

      <span style={{ ...numCell, color: empty ? '#4e5666' : '#8ab0e8' }}>
        {empty ? '—' : repo.avg_confidence.toFixed(2)}
      </span>

      {/* Cumulative buckets, so the ticks only ever climb left to right. */}
      <span data-testid="repo-activity" data-activity={buckets.map(([v]) => v).join('/')}
        title={buckets.map(([v, l]) => `${v} in ${l}`).join(' · ')}
        style={{ display: 'flex', alignItems: 'flex-end', gap: 2, height: TICK_MAX, paddingBottom: 5 }}>
        {/* An empty bucket keeps its slot but drops nearly out of sight: three
            equal stubs at tick opacity read as "no data yet", which is a
            different claim from "nothing changed here". */}
        {buckets.map(([v, l], i) => (
          <i key={l} style={{
            display: 'block', width: 5, borderRadius: 1, background: hue,
            height: tickHeight(v, maxChanges),
            opacity: v > 0 ? [1, 0.68, 0.42][i] : 0.16,
          }} />
        ))}
      </span>

      <span style={{ ...numCell, color: '#59626f' }}
        title={repo.last_commit ? new Date(repo.last_commit).toLocaleString() : undefined}>
        {repo.last_commit ? relativeTime(repo.last_commit) : '—'}
      </span>
    </div>
  );
}

export function RepoRows({ repos, dispatch }: {
  repos: LensRepoStats[];
  dispatch: Dispatch<Action>;
}) {
  if (repos.length === 0) return null;
  const rows = ranked(repos);
  const maxTotal = rows[0].total;
  // One scale across the whole list — a per-row scale would make every mount's
  // 90d tick full height and say nothing about which mount is actually moving.
  const maxChanges = Math.max(...rows.map(r => r.changes_90d), 0);

  // Picking a mount means "show me this repo": narrow the union to it and open
  // its first fact. FOCUS_LENS_SOURCE carries the whole intent — sources, sort,
  // and one nav entry — rather than the SET_LENS_SOURCES + SET_LIBRARY_SORT
  // pair it replaces, which pushed no history at all and so left Back unable to
  // give the reader their other five mounts back.
  //
  // It drives the SOURCES selection, not an ADD_FILTER repo: chip. Both narrow
  // the union and the two INTERSECT in the Library, so driving the one the left
  // panel already displays as "1 of 6 mounts" leaves ONE visible control over
  // the scope rather than two that can silently disagree.
  const pick = (name: string) => dispatch({ type: 'FOCUS_LENS_SOURCE', repo: name });

  return (
    <div data-testid="repo-rows" style={{ marginTop: 4 }}>
      <div style={{ ...labelStyle, marginBottom: 8 }}>Repos · {rows.length}</div>
      <div style={{
        display: 'grid', gridTemplateColumns: GRID, gap: 14,
        borderBottom: '1px solid #1c2029', marginBottom: 3,
      }}>
        <span style={headCell}>Mount</span>
        <span style={{ ...headCell, textAlign: 'right' }}>Facts</span>
        <span style={{ ...headCell, textAlign: 'right' }}>Conf</span>
        <span style={headCell}>7·30·90d</span>
        <span style={{ ...headCell, textAlign: 'right' }}>Updated</span>
      </div>
      {rows.map(r => (
        <RepoRow key={r.id || r.name} repo={r} maxTotal={maxTotal} maxChanges={maxChanges}
          onPick={pick} />
      ))}
    </div>
  );
}
