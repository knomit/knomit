import { useEffect, useState } from 'react';
import { fetchVersion } from './api';

// VersionBadge shows the running server's build version in the bottom-right
// corner. It fetches once on mount and renders nothing until (or unless) the
// version is available — a failed fetch leaves the UI clean rather than noisy.
export function VersionBadge() {
  const [full, setFull] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    fetchVersion()
      .then(v => { if (alive) setFull(v.full); })
      .catch(() => { /* best-effort: no badge on failure */ });
    return () => { alive = false; };
  }, []);

  if (!full) return null;

  return (
    <div
      data-testid="version-badge"
      title="knomit build version"
      style={{
        position: 'fixed',
        right: 8,
        bottom: 6,
        fontSize: 11,
        color: '#666',
        fontFamily: 'var(--mono, monospace)',
        pointerEvents: 'none',
        userSelect: 'none',
        zIndex: 10,
      }}
    >
      v{full}
    </div>
  );
}
