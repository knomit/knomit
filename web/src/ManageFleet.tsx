import { useCallback, useEffect, useState } from 'react';
import { api, type FleetMember, type FleetStatus } from './api';
import { btn, card, cardLabel } from './manageStyles';

// ManageFleet is this instance's fleet membership (F09): the state machine
// standalone -> registering -> registered -> unregistering -> standalone, as
// GET /api/v1/fleet reports it. Every error the server records (last_error) is
// shown verbatim with when it happened and whether a retry is pending; an
// action the current state forbids is disabled WITH its reason, never hidden.
//
// Registering pushes this instance's member record on its agent branch of the
// fleet repository; a human accepts it by merging that branch into the fleet's
// main (record state "pending" until then). Unregistering pushes a "left"
// record FIRST and unmounts the fleet repository only once that push lands.

// Pushes are retried on the sync loop, so the state moves without a click.
const POLL_MS = 5_000;

const mono: React.CSSProperties = { fontFamily: 'ui-monospace, monospace', fontSize: 12, wordBreak: 'break-all' };
const dim: React.CSSProperties = { color: '#888', fontSize: 12 };
const errText: React.CSSProperties = { color: '#f87171', fontSize: 12, marginTop: 6 };
const input: React.CSSProperties = { background: '#000', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '4px 6px', minWidth: 280 };

function when(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

// registerBlocked / unregisterBlocked are the reasons the state machine
// forbids an action ('' = allowed). The server refuses the same requests with
// the same codes; the UI only says so before the click.
export function registerBlocked(st: FleetStatus): string {
  switch (st.state) {
    case 'registering':
      return 'A registration is still being pushed; it is retried on every sync.';
    case 'unregistering':
      return 'An unregistration is still being pushed; it must finish (or the fleet repository be archived) before registering again.';
    case 'registered':
      return `Registered with ${st.fleet_repo ?? 'a fleet'}: unregister first to join another.`;
  }
  return '';
}

export function unregisterBlocked(st: FleetStatus): string {
  switch (st.state) {
    case 'standalone':
      return 'This instance is standalone: there is no fleet to leave.';
    case 'unregistering':
      return 'The departure is already being pushed; it is retried on every sync.';
  }
  return '';
}

export function ManageFleet() {
  const [st, setSt] = useState<FleetStatus | null>(null);
  const [members, setMembers] = useState<FleetMember[]>([]);
  // loadErr is a failed poll; actErr is the refusal of the last click. The
  // poll must never clear actErr: the user has to read why the click failed.
  const [loadErr, setLoadErr] = useState('');
  const [actErr, setActErr] = useState('');
  const [url, setUrl] = useState('');
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api.getFleet()
      .then(s => {
        setSt(s);
        setLoadErr('');
        if (s.fleet_repo) {
          api.listFleetMembers().then(setMembers).catch(() => setMembers([]));
        } else {
          setMembers([]);
        }
      })
      .catch(e => setLoadErr(e instanceof Error ? e.message : String(e)));
  }, []);

  useEffect(() => {
    load();
    const t = setInterval(load, POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  // The form keeps its values on a failure, so a failed clone is re-issued
  // with one click.
  const act = async (f: () => Promise<FleetStatus>) => {
    setBusy(true);
    setActErr('');
    try {
      setSt(await f());
    } catch (e) {
      setActErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
      load();
    }
  };

  if (!st) {
    return <div style={{ padding: '4px 2px', ...dim }}>{loadErr ? <div role="alert" style={errText}>{loadErr}</div> : 'Loading…'}</div>;
  }
  const regWhy = registerBlocked(st);
  const unregWhy = unregisterBlocked(st);
  const pending = st.state === 'registering' || st.state === 'unregistering';

  return (
    <div style={{ padding: '4px 2px' }}>
      <div style={dim}>
        The fleet this instance belongs to. Joining pushes this instance's member record on its own branch of the fleet
        repository; someone with access to that repository accepts it by merging the branch into its main.
      </div>

      <div data-testid="fleet-status" style={card}>
        <div style={cardLabel}>State</div>
        <div data-testid="fleet-state" style={{ fontSize: 16, fontWeight: 600, color: '#eee' }}>{st.state}</div>
        {st.since && <div style={dim}>since {when(st.since)}</div>}

        <div style={{ ...cardLabel, marginTop: 10 }}>This agent</div>
        <div style={mono}>{st.agent_id}</div>

        {st.fleet_repo && (
          <>
            <div style={{ ...cardLabel, marginTop: 10 }}>Fleet repository</div>
            <div style={{ color: '#ddd' }}>{st.fleet_repo}</div>
            {st.fleet_url && <div style={mono}>{st.fleet_url}</div>}
            <div style={{ ...cardLabel, marginTop: 10 }}>Record on the fleet's main</div>
            <div data-testid="fleet-record-state" style={{ color: '#ddd' }}>
              {st.record_state ?? 'pending'}
              {st.record_state === 'pending' && <span style={dim}> — waiting for someone to merge this instance's branch</span>}
            </div>
          </>
        )}

        {st.last_error && (
          <div data-testid="fleet-last-error" role="alert" style={errText}>
            Last attempt {when(st.last_attempt)} failed: <span style={mono}>{st.last_error}</span>
            {pending && <div style={dim}>Retrying on the next sync.</div>}
          </div>
        )}
      </div>

      <div style={card}>
        <div style={cardLabel}>Join a fleet</div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
          <input aria-label="Fleet repository URL" placeholder="https://… or git@…" value={url} onChange={e => setUrl(e.target.value)} style={input} />
          <input aria-label="Access token" placeholder="access token (optional)" type="password" value={token} onChange={e => setToken(e.target.value)} style={input} />
          <button type="button" data-testid="fleet-register" style={btn(busy || !!regWhy || url.trim() === '', 'primary')}
            disabled={busy || !!regWhy || url.trim() === ''} title={regWhy}
            onClick={() => act(() => api.registerFleet(url.trim(), token))}>
            Register
          </button>
        </div>
        {regWhy && <div data-testid="fleet-register-why" style={{ ...dim, marginTop: 6 }}>{regWhy}</div>}
      </div>

      <div style={card}>
        <div style={cardLabel}>Leave the fleet</div>
        <button type="button" data-testid="fleet-unregister" style={btn(busy || !!unregWhy, 'danger')}
          disabled={busy || !!unregWhy} title={unregWhy}
          onClick={() => act(() => api.unregisterFleet())}>
          Unregister
        </button>
        {unregWhy && <div data-testid="fleet-unregister-why" style={{ ...dim, marginTop: 6 }}>{unregWhy}</div>}
        {st.state === 'unregistering' && (
          <div style={{ ...dim, marginTop: 6 }}>To stop retrying, archive the fleet repository by hand.</div>
        )}
      </div>

      {actErr && <div role="alert" data-testid="fleet-error" style={errText}>{actErr}</div>}
      {loadErr && <div role="alert" style={errText}>{loadErr}</div>}

      {members.length > 0 && (
        <div style={card}>
          <div style={cardLabel}>Members (the fleet's main)</div>
          {members.map(m => (
            <div key={m.path ?? m.agent} data-testid="fleet-member" style={{ display: 'flex', gap: 10, ...mono }}>
              <span style={{ color: '#ddd' }}>{m.agent}</span>
              <span>{m.state}</span>
              {m.host && <span style={{ color: '#888' }}>{m.host}</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
