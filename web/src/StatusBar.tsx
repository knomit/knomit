import type { AppState } from './state';

interface Props { state: AppState }

export function StatusBar({ state }: Props) {
  return (
    <div style={{ height: 28, background: '#0d0d0d', borderTop: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 16px', gap: 16, flexShrink: 0 }}>
      <span style={{ color: '#444', fontSize: 11 }}>
        Embeddings: <span style={{ color: state.embeddingsEnabled ? '#8c8' : '#666' }}>{state.embeddingsEnabled ? 'on' : 'off'}</span>
      </span>
      {state.loading && <span style={{ color: '#666', fontSize: 11 }}>Loading…</span>}
    </div>
  );
}
