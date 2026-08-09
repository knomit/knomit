import { useEffect, useState } from 'react';
import { OverflowCrumb } from './OverflowCrumb';
import type { TrailCrumb, AsOf } from './state';
import { api } from './api';

const AMBER = '#f5c47a';
const AMBER_DIM = '#a36a18';

// Trails longer than this collapse to root › … › secondLast › last. Four is the
// largest trail that still reads cleanly inline.
const MAX_INLINE = 4;

function basename(factPath: string): string {
  const last = factPath.split('/').pop() ?? factPath;
  return last.endsWith('.md') ? last.slice(0, -3) : last;
}

// The commit a crumb is anchored at (undefined = HEAD/live).
function crumbCommit(asOf: AsOf): string | undefined {
  if (asOf.mode === 'history') return asOf.commit;
  if (asOf.mode === 'diff') return asOf.to;
  return undefined;
}

function crumbKey(crumb: TrailCrumb): string {
  return `${crumb.factPath}@${crumbCommit(crumb.asOf) ?? 'HEAD'}`;
}

// A breadcrumb row item: either a single crumb (by its trail index) or the
// collapsed overflow standing in for a run of hidden middle crumbs.
type Item =
  | { kind: 'crumb'; index: number }
  | { kind: 'overflow'; indices: number[] };

// Collapse the trail to root › … › secondLast › last when it overflows.
function layoutItems(count: number): Item[] {
  if (count <= MAX_INLINE) {
    return Array.from({ length: count }, (_, index) => ({ kind: 'crumb', index }));
  }
  const hidden = Array.from({ length: count - 3 }, (_, k) => k + 1); // indices 1 .. count-3
  return [
    { kind: 'crumb', index: 0 },
    { kind: 'overflow', indices: hidden },
    { kind: 'crumb', index: count - 2 },
    { kind: 'crumb', index: count - 1 },
  ];
}

interface TrailBreadcrumbProps {
  repo: string;
  branch: string;
  // In a lens context crumbs carry canonical paths (bare = write repo,
  // kb://<id12>/… = a read mount) that the repo-scoped fact endpoint cannot
  // resolve; the lens single-fact endpoint routes them to the right mount.
  lensName?: string;
  trail: TrailCrumb[];
  // Shared title cache (state.factTitles) — the RightPanel writes the title of
  // every fact it loads here, so a crumb we've navigated to is already labelled
  // WITHOUT re-fetching. This is the authoritative source; the local fetch below
  // is only a fallback for a crumb not (yet) in the cache.
  titles?: Record<string, string>;
  onJump: (index: number) => void;
}

export function TrailBreadcrumb({ repo, branch, lensName, trail, titles, onJump }: TrailBreadcrumbProps) {
  // Fallback fetch cache: for crumbs the shared cache doesn't cover. Crumbs
  // carry only path + anchor; fetch each fact's human title so the breadcrumb
  // reads "Alpha fact…", not the hash filename.
  const [fetched, setFetched] = useState<Record<string, string>>({});

  useEffect(() => {
    let cancelled = false;
    for (const crumb of trail) {
      const key = crumbKey(crumb);
      // Already labelled — the RightPanel cached this fact's title when we
      // navigated to it (the common case, and the only one that resolves a
      // retracted fact, whose live single-fact endpoint 404s a tombstone).
      if (titles?.[key] !== undefined || fetched[key] !== undefined) continue;
      const commit = crumbCommit(crumb.asOf);
      const fetchTitle = lensName
        ? api.getLensFact(lensName, crumb.factPath)
        : api.fact(repo, branch, crumb.factPath, commit, commit ? { fallback: 'before' } : undefined);
      fetchTitle
        .then(f => {
          if (!cancelled && f?.title) setFetched(prev => ({ ...prev, [key]: f.title }));
        })
        .catch(() => { /* leave fallback to basename */ });
    }
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, branch, lensName, trail, titles]);

  const label = (index: number) => {
    const key = crumbKey(trail[index]);
    return titles?.[key] ?? fetched[key] ?? basename(trail[index].factPath);
  };
  const items = layoutItems(trail.length);

  return (
    <div style={{ flexShrink: 0, position: 'relative' }}>
      <div style={{
        display: 'flex', alignItems: 'center', padding: '6px 14px',
        background: '#0d0d0d', borderBottom: '1px solid #222', minHeight: 38, gap: 8,
      }}>
        {/* Crumb trail — collapses to root › … › last two when it overflows */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 5, flex: 1, minWidth: 0, flexWrap: 'nowrap' }}>
          {items.map((item, pos) => (
            <span key={item.kind === 'crumb' ? `c${item.index}` : 'overflow'} style={{ display: 'inline-flex', alignItems: 'center', gap: 5, flexShrink: 0, minWidth: 0 }}>
              {pos > 0 && (
                <span style={{ color: '#555', flexShrink: 0, fontSize: 12 }}>›</span>
              )}
              {item.kind === 'crumb' ? (
                <CrumbButton
                  label={label(item.index)}
                  isLast={item.index === trail.length - 1}
                  onClick={() => onJump(item.index)}
                />
              ) : (
                <OverflowCrumb
                  indices={item.indices}
                  label={label}
                  onJump={onJump}
                />
              )}
            </span>
          ))}
        </div>
      </div>

      {/* Amber gradient bottom rule */}
      <div style={{
        height: 2,
        background: `linear-gradient(90deg, ${AMBER_DIM} 0%, ${AMBER} 50%, ${AMBER_DIM} 100%)`,
        opacity: 0.75,
      }} />
    </div>
  );
}

function CrumbButton({ label, isLast, onClick }: { label: string; isLast: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      title={label}
      style={{
        cursor: 'pointer',
        background: 'none',
        border: 'none',
        padding: '1px 4px',
        fontSize: 11.5,
        color: isLast ? '#ddd' : '#888',
        whiteSpace: 'nowrap',
        overflow: 'hidden',
        textOverflow: 'ellipsis',
        maxWidth: isLast ? 260 : 160,
      }}
    >
      {label}
    </button>
  );
}
