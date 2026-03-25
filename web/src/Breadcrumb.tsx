import type { Dispatch } from 'react';
import type { AppState, Action, View } from './state';
import { currentPath } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

// Lucide-style SVG outline icons
const TreeIcon = ({ color }: { color: string }) => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke={color} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <rect x="3" y="3" width="7" height="5" rx="1"/>
    <rect x="14" y="3" width="7" height="5" rx="1"/>
    <rect x="14" y="16" width="7" height="5" rx="1"/>
    <rect x="3" y="16" width="7" height="5" rx="1"/>
    <line x1="6.5" y1="8" x2="6.5" y2="16"/>
    <line x1="17.5" y1="8" x2="17.5" y2="16"/>
  </svg>
);

const ChronoIcon = ({ color }: { color: string }) => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke={color} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <circle cx="12" cy="12" r="10"/>
    <polyline points="12 6 12 12 16 14"/>
  </svg>
);

const HistoryIcon = ({ color }: { color: string }) => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke={color} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <circle cx="12" cy="6" r="3"/>
    <circle cx="12" cy="18" r="3"/>
    <line x1="12" y1="9" x2="12" y2="15"/>
  </svg>
);

const viewButtons: { view: View; Icon: typeof TreeIcon; label: string }[] = [
  { view: 'tree',    Icon: TreeIcon,    label: 'Tree' },
  { view: 'chrono',  Icon: ChronoIcon,  label: 'Recent' },
  { view: 'history', Icon: HistoryIcon, label: 'History' },
];

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
      minHeight: 28,
    }}>
      {/* Breadcrumb segments */}
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
              onClick={isLast ? undefined : () => dispatch({ type: 'ADD_FILTER', chip: { category: 'path', value: segmentPath } })}
            >
              {part}
            </span>
          </span>
        );
      })}

      <div style={{ flex: 1 }} />

      {/* View switcher — right-aligned */}
      <div style={{ display: 'flex', gap: 2, background: '#252525', borderRadius: 4, padding: 2 }}>
        {viewButtons.map(({ view, Icon, label }) => {
          const active = state.view === view;
          return (
            <button
              key={view}
              onClick={() => dispatch({ type: 'SET_VIEW', view })}
              title={label}
              style={{
                background: active ? '#444' : 'transparent',
                border: 'none',
                padding: '3px 8px',
                borderRadius: 3,
                cursor: 'pointer',
                display: 'flex',
                alignItems: 'center',
              }}
            >
              <Icon color={active ? '#eee' : '#666'} />
            </button>
          );
        })}
      </div>
    </div>
  );
}
