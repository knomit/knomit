import { useCallback, useEffect, useState } from 'react';
import { api, type FleetStatus } from './api';

// The fleet's pages and the rail all read GET /fleet; it changes without a
// click (the master's round admits a record, a sync fails), so it is polled.
export const FLEET_POLL_MS = 5_000;

/**
 * useFleet is GET /fleet, polled app-wide: the rail draws the Fleet section
 * from it, the crumb its chip, and the fleet target's pages read it. `fleet`
 * is null until the first answer; a failed poll keeps the last answer and
 * reports `error`. `reload` re-reads at once (after a create, a join, a
 * leave), and resolves with what it read.
 */
export function useFleet(enabled = true): { fleet: FleetStatus | null; error: string; reload: () => Promise<FleetStatus | null> } {
  const [fleet, setFleet] = useState<FleetStatus | null>(null);
  const [error, setError] = useState('');
  const reload = useCallback(() => api.getFleet()
    .then(st => { setFleet(st); setError(''); return st; })
    .catch(e => { setError(e instanceof Error ? e.message : String(e)); return null; }), []);
  useEffect(() => {
    if (!enabled) return;
    void reload();
    const t = setInterval(() => { void reload(); }, FLEET_POLL_MS);
    return () => clearInterval(t);
  }, [enabled, reload]);
  return { fleet, error, reload };
}
