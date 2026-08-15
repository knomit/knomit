import type { CreateEvent } from './api';

// CreateProgress renders the NDJSON event stream from api.createRepo — the
// same rendering CreateRepoForm did inline, pulled out so the wizard shell
// can drop it under whichever step is showing (only ever 'review', but kept
// presentational per the step-component convention).
export function CreateProgress({ events }: { events: CreateEvent[] }) {
  if (events.length === 0) return null;
  return (
    <div data-testid="create-progress" style={box}>
      {events.map((e, i) => (
        <div key={i} style={{ color: e.type === 'error' ? '#f88' : '#9c9' }}>
          {e.type === 'progress' ? `${e.pct ?? 0}% ${e.step ?? ''} — ${e.message ?? ''}` : e.type}
        </div>
      ))}
    </div>
  );
}

const box: React.CSSProperties = { marginTop: 12, padding: 10, background: '#0c0c0c', borderRadius: 4, fontSize: 12, fontFamily: 'var(--k-font-mono)', maxHeight: 160, overflow: 'auto' };
