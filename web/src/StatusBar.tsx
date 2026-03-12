import type { AppState } from './state';

interface Props { state: AppState }

export function StatusBar({ state }: Props) {
  return (
    <div style={{ height: 28, background: '#0d0d0d', borderTop: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 16px', gap: 16, flexShrink: 0 }}>
      {state.loading && <span style={{ color: '#666', fontSize: 11 }}>Loading…</span>}
    </div>
  );
}
