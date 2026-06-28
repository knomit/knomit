import type { LibrarySort } from './state';

interface Props {
  count: number;
  scoped: boolean;
  sort: LibrarySort;
  searchActive: boolean;
  onSortChange: (sort: LibrarySort) => void;
}

const segments: { value: LibrarySort; label: string; testid: string }[] = [
  { value: 'path',      label: 'Path',      testid: 'sort-path' },
  { value: 'recent',    label: 'Recent',    testid: 'sort-recent' },
  { value: 'relevance', label: 'Relevance', testid: 'sort-relevance' },
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
      {/* Sort toggle in the same bordered-mono vocabulary as the history rail's
          "↩ live" action: active = accent text + accent border, no dark pill. */}
      <div style={{ display: 'flex', gap: 6 }}>
        {visible.map(seg => {
          const active = sort === seg.value;
          // While searching, order is forced to relevance — Path/Recent can't
          // change it and clicking one only resets the open fact. Disable them
          // so Relevance is the only live control.
          const disabled = searchActive && seg.value !== 'relevance';
          return (
            <button
              key={seg.value}
              data-testid={seg.testid}
              disabled={disabled}
              onClick={() => { if (!disabled) onSortChange(seg.value); }}
              title={disabled ? 'Sorting is disabled while searching' : undefined}
              style={{
                background: 'none',
                color: disabled ? '#3a3a3a' : active ? '#7c9' : '#666',
                border: `1px solid ${active ? '#7c995c' : 'transparent'}`,
                outline: 'none',
                padding: '2px 8px', borderRadius: 4,
                cursor: disabled ? 'not-allowed' : 'pointer',
                fontSize: 10, fontFamily: 'var(--k-font-mono)', letterSpacing: 1, textTransform: 'uppercase',
              }}
            >{seg.label}</button>
          );
        })}
      </div>
    </div>
  );
}
