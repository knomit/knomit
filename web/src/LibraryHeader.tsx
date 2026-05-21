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
        fontSize: 11, color: '#888', fontFamily: 'monospace',
      }}
    >
      <span>
        {count} facts <span style={{ color: '#555' }}>· {scoped ? 'scoped' : 'global'}</span>
      </span>
      <div style={{ display: 'flex', gap: 2, background: '#1a1a1a', borderRadius: 4, padding: 2 }}>
        {visible.map(seg => {
          const active = sort === seg.value;
          return (
            <button
              key={seg.value}
              data-testid={seg.testid}
              onClick={() => onSortChange(seg.value)}
              style={{
                background: active ? '#252535' : 'transparent',
                color: active ? '#eee' : '#666',
                border: 'none', padding: '2px 8px', borderRadius: 3,
                cursor: 'pointer', fontSize: 10, fontFamily: 'monospace',
              }}
            >{seg.label}</button>
          );
        })}
      </div>
    </div>
  );
}
