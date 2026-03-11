import type { AppState } from './state';

interface Props { state: AppState }

export function StatusBar({ state }: Props) {
  return (
    <div style={{ height: 24, background: '#0d0d0d', borderTop: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 12px', flexShrink: 0 }}>
      {state.loading && <span style={{ color: '#666', fontSize: 11 }}>Loading…</span>}
      <div style={{ flex: 1 }} />
      <span
        title={state.embeddingsEnabled ? 'Embeddings on' : 'Embeddings off'}
        style={{ color: state.embeddingsEnabled ? '#8c8' : '#555', fontSize: 11, fontFamily: 'monospace', fontWeight: 'bold', userSelect: 'none' }}>
        e
      </span>
    </div>
  );
}
