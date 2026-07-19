import { useEffect, useRef, useState } from 'react';
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
  onJump: (index: number) => void;
}

export function TrailBreadcrumb({ repo, branch, lensName, trail, onJump }: TrailBreadcrumbProps) {
  // Crumbs carry only path + anchor; fetch each fact's human title (cached by
  // path@commit) so the breadcrumb reads "Alpha fact…", not the hash filename.
  const [titles, setTitles] = useState<Record<string, string>>({});

  useEffect(() => {
    let cancelled = false;
    for (const crumb of trail) {
      const key = crumbKey(crumb);
      if (titles[key] !== undefined) continue;
      const commit = crumbCommit(crumb.asOf);
      // Lens context: fetch the LIVE title through the lens endpoint. Anchoring
      // the title at the crumb's commit would need an id12→mount mapping the
      // client doesn't keep; titles are stable enough for breadcrumb chrome and
      // a failure already falls back to the basename.
      const fetchTitle = lensName
        ? api.getLensFact(lensName, crumb.factPath)
        : api.fact(repo, branch, crumb.factPath, commit, commit ? { fallback: 'before' } : undefined);
      fetchTitle
        .then(f => {
          if (!cancelled && f?.title) setTitles(prev => ({ ...prev, [key]: f.title }));
        })
        .catch(() => { /* leave fallback to basename */ });
    }
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repo, branch, lensName, trail]);

  const label = (index: number) => titles[crumbKey(trail[index])] ?? basename(trail[index].factPath);
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

// The collapsed "…" standing in for hidden middle crumbs; opens a dropdown
// listing them (in order) so any can be jumped to directly.
function OverflowCrumb({ indices, label, onJump }: {
  indices: number[];
  label: (index: number) => string;
  onJump: (index: number) => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('mousedown', onDown);
    return () => {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('mousedown', onDown);
    };
  }, [open]);

  return (
    <span ref={ref} style={{ position: 'relative', display: 'inline-flex', alignItems: 'center' }}>
      <button
        data-testid="crumb-overflow"
        onClick={() => setOpen(o => !o)}
        title={`${indices.length} more`}
        aria-haspopup="menu"
        aria-expanded={open}
        style={{
          cursor: 'pointer', background: 'none', border: 'none',
          padding: '1px 4px', fontSize: 11.5, color: '#888', lineHeight: 1,
        }}
      >
        …
      </button>
      {open && (
        <div
          data-testid="crumb-overflow-menu"
          role="menu"
          style={{
            position: 'absolute', top: '100%', left: 0, marginTop: 4, zIndex: 30,
            background: '#161616', border: '1px solid #333', borderRadius: 6,
            padding: 4, minWidth: 180, maxWidth: 320,
            boxShadow: '0 6px 18px rgba(0,0,0,0.5)',
            maxHeight: 280, overflowY: 'auto',
          }}
        >
          {indices.map(index => (
            <button
              key={index}
              role="menuitem"
              onClick={() => { onJump(index); setOpen(false); }}
              title={label(index)}
              style={{
                display: 'block', width: '100%', textAlign: 'left',
                cursor: 'pointer', background: 'none', border: 'none',
                padding: '5px 8px', borderRadius: 4, fontSize: 11.5, color: '#bbb',
                whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
              }}
              onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = '#222'; }}
              onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = 'none'; }}
            >
              {label(index)}
            </button>
          ))}
        </div>
      )}
    </span>
  );
}
