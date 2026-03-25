import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import { currentPath } from './state';

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
      padding: '4px 12px',
      background: '#1a1a1a',
      borderBottom: '1px solid #333',
      fontSize: 13,
      minHeight: 24,
    }}>
      {parts.map((part, i) => {
        const isLast = i === parts.length - 1;
        const segmentPath = parts.slice(0, i + 1).join('/');
        return (
          <span key={i} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
            {i > 0 && <span style={{ color: '#555' }}>/</span>}
            <span
              style={{
                color: isLast ? '#eee' : '#888',
                cursor: isLast ? 'default' : 'pointer',
                fontWeight: isLast ? 500 : 400,
              }}
              onClick={isLast ? undefined : () => dispatch({ type: 'NAVIGATE', path: segmentPath })}
            >
              {part}
            </span>
          </span>
        );
      })}
    </div>
  );
}
