import type { LibrarySort } from './state';
import type { ComponentType } from 'react';
import { TreeIcon, StopwatchIcon, TargetIcon } from './icons';

interface Props {
  count: number;
  scoped: boolean;
  sort: LibrarySort;
  searchActive: boolean;
  onSortChange: (sort: LibrarySort) => void;
}

type IconType = ComponentType<{ color: string; size?: number }>;

// Sort axes render as theme-colored glyphs (tree = path hierarchy, stopwatch =
// recency, target = best-match relevance); the label survives as the tooltip
// and accessible name.
const segments: { value: LibrarySort; label: string; testid: string; Icon: IconType }[] = [
  { value: 'path',      label: 'Path',      testid: 'sort-path',      Icon: TreeIcon },
  { value: 'recent',    label: 'Recent',    testid: 'sort-recent',    Icon: StopwatchIcon },
  { value: 'relevance', label: 'Relevance', testid: 'sort-relevance', Icon: TargetIcon },
];

export function LibraryHeader({ count, scoped, sort, searchActive, onSortChange }: Props) {
  const visible = segments.filter(s => s.value !== 'relevance' || searchActive);
  return (
    <div
      data-testid="library-header"
      style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        padding: '6px 12px', borderBottom: '1px solid #1a1a1a', background: '#0f0f0f',
        fontSize: 11, color: '#888', fontFamily: 'var(--k-font-mono)',
      }}
    >
      {/* Live panel title — mirrors the history rail's "TIMELINE · N versions",
          in the live green so the two modes share one header shape. */}
      <span style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
        <span style={{ color: '#7c9', letterSpacing: 1, textTransform: 'uppercase' }}>Library</span>
        <span style={{ color: '#666' }}>· {count} facts · {scoped ? 'scoped' : 'global'}</span>
      </span>
      {/* Sort axes as borderless glyphs — state reads through color alone:
          accent green = active, muted = idle, brighter on hover. No pill, no
          selection border. The icon inherits the button's CSS color. */}
      <div style={{ display: 'flex', gap: 4 }}>
        {visible.map(seg => {
          const active = sort === seg.value;
          // While searching, order is forced to relevance — Path/Recent can't
          // change it and clicking one only resets the open fact. Disable them
          // so Relevance is the only live control.
          const disabled = searchActive && seg.value !== 'relevance';
          const color = disabled ? '#3a3a3a' : active ? '#7c9' : '#666';
          return (
            <button
              key={seg.value}
              data-testid={seg.testid}
              disabled={disabled}
              onClick={() => { if (!disabled) onSortChange(seg.value); }}
              aria-label={`Sort by ${seg.label}`}
              aria-pressed={active}
              title={disabled ? 'Sorting is disabled while searching' : `Sort by ${seg.label}`}
              onMouseEnter={e => { if (!disabled && !active) e.currentTarget.style.color = '#aaa'; }}
              onMouseLeave={e => { if (!disabled && !active) e.currentTarget.style.color = color; }}
              style={{
                background: 'none',
                color,
                border: 'none',
                outline: 'none',
                padding: '3px 5px', borderRadius: 4,
                cursor: disabled ? 'not-allowed' : 'pointer',
                display: 'inline-flex', alignItems: 'center',
              }}
            ><seg.Icon color="currentColor" size={14} /></button>
          );
        })}
      </div>
    </div>
  );
}
