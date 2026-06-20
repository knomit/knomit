import { useEffect, useState } from 'react';
import type { TrailCrumb, AsOf } from './state';
import { api } from './api';

const AMBER = '#f5c47a';
const AMBER_DIM = '#a36a18';

function basename(factPath: string): string {
  const last = factPath.split('/').pop() ?? factPath;
  return last.endsWith('.md') ? last.slice(0, -3) : last;
}

// The commit a crumb is anchored at (undefined = HEAD/live).
function crumbCommit(asOf: AsOf): string | undefined {
  if (asOf.mode === 'scrubbed') return asOf.commit;
  if (asOf.mode === 'diff') return asOf.to;
  return undefined;
}

function crumbKey(crumb: TrailCrumb): string {
  return `${crumb.factPath}@${crumbCommit(crumb.asOf) ?? 'HEAD'}`;
}

interface TrailBreadcrumbProps {
  repo: string;
  branch: string;
  trail: TrailCrumb[];
  onJump: (index: number) => void;
  onReturnToNow: () => void;
}

export function TrailBreadcrumb({ repo, branch, trail, onJump, onReturnToNow }: TrailBreadcrumbProps) {
  // Crumbs carry only path + anchor; fetch each fact's human title (cached by
  // path@commit) so the breadcrumb reads "Alpha fact…", not the hash filename.
  const [titles, setTitles] = useState<Record<string, string>>({});

  useEffect(() => {
    let cancelled = false;
    for (const crumb of trail) {
      const key = crumbKey(crumb);
      if (titles[key] !== undefined) continue;
      const commit = crumbCommit(crumb.asOf);
      api.fact(repo, branch, crumb.factPath, commit, commit ? { fallback: 'before' } : undefined)
        .then(f => {
          if (!cancelled && f?.title) setTitles(prev => ({ ...prev, [key]: f.title }));
        })
        .catch(() => { /* leave fallback to basename */ });
    }
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, branch, trail]);

  return (
    <div style={{ flexShrink: 0, position: 'relative' }}>
      <div style={{
        display: 'flex', alignItems: 'center', padding: '6px 14px',
        background: '#0d0d0d', borderBottom: '1px solid #222', minHeight: 38, gap: 8,
      }}>
        {/* Crumb trail — scrolls horizontally when it overflows */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 5, flex: 1, minWidth: 0, overflowX: 'auto', flexWrap: 'nowrap' }}>
          {trail.map((crumb, i) => {
            const label = titles[crumbKey(crumb)] ?? basename(crumb.factPath);
            const isLast = i === trail.length - 1;
            return (
              <span key={i} style={{ display: 'inline-flex', alignItems: 'center', gap: 5, flexShrink: 0 }}>
                {i > 0 && (
                  <span style={{ color: '#555', flexShrink: 0, fontSize: 12 }}>›</span>
                )}
                <button
                  onClick={() => onJump(i)}
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
              </span>
            );
          })}
        </div>

        {/* Amber "reading history · read-only" label + return to now */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0 }}>
          <span style={{
            color: AMBER,
            fontFamily: 'monospace',
            fontSize: 10,
            letterSpacing: 0.5,
            textTransform: 'uppercase',
            whiteSpace: 'nowrap',
          }}>
            reading history · read-only
          </span>
          <button
            onClick={onReturnToNow}
            style={{
              cursor: 'pointer',
              whiteSpace: 'nowrap',
              background: '#1a1a1a',
              border: `1px solid #333`,
              borderRadius: 4,
              color: '#aaa',
              fontFamily: 'monospace',
              fontSize: 10,
              padding: '3px 8px',
            }}
          >
            return to now
          </button>
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
