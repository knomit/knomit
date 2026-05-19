import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import { currentPath, isLive } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function Breadcrumb({ state, dispatch }: Props) {
  const path = currentPath(state);
  const parts = path.split('/');

  return (
    <div style={{
      display: 'flex',
      alignItems: 'center',
      gap: 4,
      padding: '0 12px',
      background: '#111',
      borderBottom: isLive(state) ? '1px solid #1c1c1c' : '1px solid #e5a23c',
      fontSize: 13,
      height: 34,
      flexShrink: 0,
    }}>
      {/* Breadcrumb segments */}
      {parts.map((part, i) => {
        const isLast = i === parts.length - 1;
        const segmentPath = parts.slice(0, i + 1).join('/');
        return (
          <span key={i} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
            {i > 0 && <span style={{ color: '#333' }}>/</span>}
            <span
              style={{
                color: isLast ? '#ccc' : '#555',
                cursor: isLast ? 'default' : 'pointer',
                fontWeight: isLast ? 500 : 400,
                fontSize: 12,
              }}
              onClick={isLast ? undefined : () => dispatch({ type: 'ADD_FILTER', chip: { category: 'path', value: segmentPath } })}
            >
              {part}
            </span>
          </span>
        );
      })}

      <div style={{ flex: 1 }} />

      <span style={{
        color: '#555', fontSize: 10, fontFamily: 'monospace',
        letterSpacing: 0.5, textTransform: 'uppercase',
      }}>Library</span>
    </div>
  );
}
