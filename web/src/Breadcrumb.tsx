import type { Dispatch } from 'react';
import type { AppState, Action, View } from './state';
import { currentPath, isLive } from './state';
import type { NavRequest } from './useNavigationManager';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
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

const shortcuts: Record<View, string> = { tree: '1', chrono: '2', history: '3' };

export function Breadcrumb({ state, dispatch, navigate }: Props) {
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

      {/* View switcher — segmented control */}
      <div style={{ display: 'flex', gap: 2, background: '#1a1a1a', borderRadius: 5, padding: 3 }}>
        {viewButtons.map(({ view, Icon, label }) => {
          const active = state.view === view;
          return (
            <button
              key={view}
              onClick={() => navigate({ view })}
              title={`${label} (${shortcuts[view]})`}
              style={{
                background: active ? '#252535' : 'transparent',
                border: 'none',
                outline: 'none',
                padding: '3px 10px',
                borderRadius: 3,
                cursor: 'pointer',
                display: 'flex',
                alignItems: 'center',
                gap: 5,
                color: active ? '#eee' : '#555',
                fontSize: 11,
                transition: 'background 0.12s, color 0.12s',
              }}
            >
              <Icon color="currentColor" />
              {label}
              <span style={{
                fontSize: 8,
                color: active ? '#666' : '#3a3a3a',
                background: active ? '#111' : '#111',
                border: `1px solid ${active ? '#333' : '#252525'}`,
                borderRadius: 2,
                padding: '0 3px',
                lineHeight: '13px',
                fontFamily: 'monospace',
              }}>{shortcuts[view]}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}
