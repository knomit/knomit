import type { TrailCrumb } from './state';

const AMBER = '#f5c47a';
const AMBER_DIM = '#a36a18';

function basename(factPath: string): string {
  const last = factPath.split('/').pop() ?? factPath;
  return last.endsWith('.md') ? last.slice(0, -3) : last;
}

interface TrailBreadcrumbProps {
  trail: TrailCrumb[];
  onJump: (index: number) => void;
  onReturnToNow: () => void;
}

export function TrailBreadcrumb({ trail, onJump, onReturnToNow }: TrailBreadcrumbProps) {
  return (
    <div style={{ flexShrink: 0, position: 'relative' }}>
      <div style={{
        display: 'flex', alignItems: 'center', padding: '6px 14px',
        background: '#0d0d0d', borderBottom: '1px solid #222', minHeight: 38, gap: 8,
      }}>
        {/* Crumb trail */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 5, flex: 1, minWidth: 0, overflow: 'hidden' }}>
          {trail.map((crumb, i) => (
            <span key={i} style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}>
              {i > 0 && (
                <span style={{ color: '#555', flexShrink: 0, fontSize: 12 }}>›</span>
              )}
              <button
                onClick={() => onJump(i)}
                style={{
                  cursor: 'pointer',
                  background: 'none',
                  border: 'none',
                  padding: '1px 4px',
                  fontSize: 11.5,
                  color: i === trail.length - 1 ? '#ddd' : '#888',
                  fontFamily: 'monospace',
                  whiteSpace: 'nowrap',
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  maxWidth: i === trail.length - 1 ? 320 : 150,
                }}
              >
                {basename(crumb.factPath)}
              </button>
            </span>
          ))}
        </div>

        {/* Amber "reading history · read-only" label + return to now */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0 }}>
          <span style={{
            color: AMBER,
            fontFamily: 'monospace',
            fontSize: 10,
            letterSpacing: 0.5,
            textTransform: 'uppercase',
            whiteSpace: 'nowrap',
          }}>
            reading history · read-only
          </span>
          <button
            onClick={onReturnToNow}
            style={{
              cursor: 'pointer',
              whiteSpace: 'nowrap',
              background: '#1a1a1a',
              border: `1px solid #333`,
              borderRadius: 4,
              color: '#aaa',
              fontFamily: 'monospace',
              fontSize: 10,
              padding: '3px 8px',
            }}
          >
            return to now
          </button>
        </div>
      </div>

      {/* Amber gradient bottom rule */}
      <div style={{
        height: 2,
        background: `linear-gradient(90deg, ${AMBER_DIM} 0%, ${AMBER} 50%, ${AMBER_DIM} 100%)`,
        opacity: 0.75,
      }} />
    </div>
  );
}
