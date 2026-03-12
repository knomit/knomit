import type { AppState } from './state';

interface Props { state: AppState }

export function StatusBar({ state }: Props) {
  const msg = state.statusMessage || (state.loading ? 'Loading…' : '');
  return (
    <div style={{ height: 24, background: '#0d0d0d', borderTop: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 12px', flexShrink: 0, gap: 8 }}>
      {msg && <span style={{ color: '#888', fontSize: 11, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{msg}</span>}
      <div style={{ flex: 1 }} />
      <span
        title={state.embeddingsEnabled ? 'Embeddings on' : 'Embeddings off'}
        style={{ color: state.embeddingsEnabled ? '#8c8' : '#555', fontSize: 11, fontFamily: 'monospace', fontWeight: 'bold', userSelect: 'none' }}>
        e
      </span>
    </div>
  );
}
