import { useCallback, useEffect, useState, type CSSProperties } from 'react';
import { api, type FleetEvent, type FleetMember, type FleetStatus } from './api';
import { FacetToolbar } from './FacetToolbar';
import { Chip } from './fleetParts';
import { EVENT_LABEL, EVENT_TONE, shortTip, when } from './fleetText';
import { FLEET_POLL_MS } from './useFleet';
import { tableWrap, th, policyFooter } from './tableStyles';

/**
 * FleetActivity is the fleet target's Activity tab: what happened in the
 * fleet, newest first, from GET /fleet/activity (main's history, the
 * master's issuance log, the CRL and the master's judge). Polled like
 * Members; it is not a log stream. A refused event's agent is the refused
 * tip's claimed author (#527 F2), and says so.
 */
export function FleetActivity({ fleet }: { fleet: FleetStatus }) {
  const [events, setEvents] = useState<FleetEvent[] | null>(null);
  const [hosts, setHosts] = useState<Record<string, string>>({});
  const [loadErr, setLoadErr] = useState('');
  const load = useCallback(() => {
    api.listFleetActivity()
      .then(ev => { setEvents(ev); setLoadErr(''); })
      .catch(e => setLoadErr(e instanceof Error ? e.message : String(e)));
    // The members name each agent's host, as the records on main say.
    api.listFleetMembers()
      .then((ms: FleetMember[]) => setHosts(Object.fromEntries(ms.filter(m => m.host).map(m => [m.agent, m.host as string]))))
      .catch(() => { /* the host is a nicety; the events stand alone */ });
  }, []);
  useEffect(() => {
    load();
    const t = setInterval(load, FLEET_POLL_MS);
    return () => clearInterval(t);
  }, [load, fleet.fleet_repo]);

  const evs = events ?? [];
  const refusals = evs.filter(e => e.kind === 'refused').length;
  return (
    <div data-testid="fleet-activity" style={body}>
      <FacetToolbar testId="fleet-activity-toolbar">
        <span style={title}>Fleet activity</span>
        <span data-testid="fleet-activity-counts" style={counts}>{events ? `${evs.length} events · ${refusals} refusals` : ''}</span>
        <span style={source}>from the fleet repository's main, its CRL and the master's judge</span>
      </FacetToolbar>
      {loadErr && <p role="alert" style={errorLine}>{loadErr}</p>}
      <div style={tableWrap}>
        <table style={table}>
          <thead>
            <tr><th style={thCell}>When</th><th style={thCell}>Event</th><th style={thCell}>Instance</th><th style={thCell}>Detail</th></tr>
          </thead>
          <tbody>
            {events && evs.length === 0 && (
              <tr><td colSpan={4} style={{ ...td, color: '#888' }}>Nothing yet.</td></tr>
            )}
            {evs.map((e, i) => (
              <tr key={`${e.at}-${e.kind}-${e.agent}-${i}`} data-testid="fleet-event" data-kind={e.kind}>
                <td style={{ ...td, whiteSpace: 'nowrap' }} title={e.at}>{when(e.at)}</td>
                <td style={td}><Chip text={EVENT_LABEL[e.kind] ?? e.kind} tone={EVENT_TONE[e.kind] ?? 'neutral'} /></td>
                <td style={{ ...td, fontFamily: 'var(--k-font-mono)', fontSize: 11.5 }}>
                  {e.agent || '—'}
                  <span style={subLine}>{e.kind === 'refused' && e.agent ? 'claimed author' : hosts[e.agent] ?? ''}</span>
                </td>
                <td style={{ ...td, ...(e.kind === 'refused' ? { color: '#e2c07a' } : {}) }}>
                  {e.kind === 'refused' && e.branch ? `${e.branch} @ ${shortTip(e.tip ?? '')}: ${e.detail}` : e.detail}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p style={policyFooter}>
        Enrolments are read from the issuance log on the master, joins and admissions from merges into main, refusals from the master's judge, revocations from the CRL. Refreshes every 5 seconds.
      </p>
    </div>
  );
}

const body: CSSProperties = { flex: '1 1 0', minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const title: CSSProperties = { color: '#ddd', fontWeight: 500, fontSize: 12, flexShrink: 0 };
const counts: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#888', flexShrink: 0 };
const source: CSSProperties = { marginLeft: 'auto', color: '#666', fontSize: 12, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const errorLine: CSSProperties = { color: '#f87171', fontSize: 12, margin: '6px 12px 0' };
const table: CSSProperties = { width: '100%', borderCollapse: 'collapse', fontSize: 12, minWidth: 640 };
const thCell: CSSProperties = {
  ...th, padding: '8px 10px 6px 0', textAlign: 'left', fontWeight: 500, color: '#555',
  textTransform: 'uppercase', letterSpacing: 1, fontSize: 9.5, whiteSpace: 'nowrap',
};
const td: CSSProperties = { padding: '7px 10px 7px 0', borderBottom: '1px solid #1a1a1a', verticalAlign: 'top', color: '#ccc' };
const subLine: CSSProperties = { display: 'block', color: '#666', fontSize: 11, marginTop: 2 };
