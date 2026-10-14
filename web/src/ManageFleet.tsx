import { useCallback, useEffect, useState } from 'react';
import { api, type FleetMember, type FleetStatus } from './api';
import { btn } from './manageStyles';
import { CopyIcon } from './icons';
import { SettingsPage, type Section } from './SettingsPage';

// The Fleet server page: this instance's fleet membership (F09): the state machine
// standalone -> registering -> registered -> unregistering -> standalone, as
// GET /api/v1/fleet reports it. Every error the server records (last_error) is
// shown verbatim with when it happened and whether a retry is pending; an
// action shown while the state forbids it is disabled WITH its reason.
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

const copyBtn: React.CSSProperties = { background: 'none', border: 'none', padding: 2, cursor: 'pointer', display: 'flex', alignItems: 'center', flexShrink: 0 };

// AgentIdCopy copies this instance's agent id (e.g. for `knomit identity
// enroll`) exactly as the server reports it: the id is opaque, so nothing is
// trimmed or reformatted. Same affordance as the connect panel's copy buttons.
function AgentIdCopy({ id }: { id: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(id)
      .then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); })
      .catch(() => { /* clipboard unavailable — nothing to recover */ });
  };
  return (
    <button type="button" data-testid="fleet-agent-id-copy" title={copied ? 'Copied' : 'Copy'} aria-label="Copy agent id"
      style={copyBtn} onClick={copy}>
      <CopyIcon color={copied ? '#7c9' : '#888'} size={13} />
    </button>
  );
}

function when(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

// capabilitiesText shows capabilities as the record has them: os/arch first,
// then the version, then anything else, sorted. Nothing is interpreted.
export function capabilitiesText(c?: Record<string, string>): string {
  if (!c) return '';
  const parts: string[] = [];
  if (c.os || c.arch) parts.push(`${c.os ?? '?'}/${c.arch ?? '?'}`);
  if (c.version) parts.push(c.version);
  for (const k of Object.keys(c).sort()) {
    if (k === 'os' || k === 'arch' || k === 'version') continue;
    parts.push(c[k] === '' ? k : `${k}=${c[k]}`);
  }
  return parts.join(' · ');
}

// recordSync is the own record's standing: the config changed since it was
// written, it waits to be merged, or it is current.
export function recordSync(st: FleetStatus): { text: string; warn: boolean } | null {
  if (!st.record) return null;
  if (st.record_current === false) {
    return { text: 'differs from knomit.toml / this binary: restart or re-register to update it', warn: true };
  }
  if (st.record_pending_update) {
    return { text: "pending update: waiting for someone to merge this instance's branch into the fleet's main", warn: false };
  }
  return { text: 'current', warn: false };
}

// registerBlocked / unregisterBlocked are the reasons the state machine
// forbids an action ('' = allowed). The server refuses the same requests with
// the same codes; the UI only says so before the click.
export function registerBlocked(st: FleetStatus): string {
  switch (st.state) {
    case 'registering':
      return 'A registration is still being pushed; it is retried on every sync.';
    case 'unregistering':
      return 'An unregistration is still being pushed; it is retried on every sync and must finish before registering again.';
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

// Which action blocks the page shows. A standalone instance can only join and
// a registered one can only leave, so each shows just the one that applies.
// While a registration or an unregistration is still being pushed both stay,
// each disabled with its reason where the state forbids it: that is when the
// reader most needs to see why the button they expect does not work.
function fleetActions(state: FleetStatus['state']): ('join' | 'leave')[] {
  if (state === 'standalone') return ['join'];
  if (state === 'registered') return ['leave'];
  return ['join', 'leave'];
}

// FleetPage is the Fleet server page: the membership as ruled blocks with an
// index beside them (This instance, Members, then Join or Leave by state).
export function FleetPage() {
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

  const sections: Section[] = [];
  if (!st) {
    sections.push({
      id: 'this-instance', title: 'This instance',
      body: loadErr ? <div role="alert" style={errText}>{loadErr}</div> : <div style={dim}>Loading…</div>,
    });
    return <SettingsPage kind="fleet" sections={sections} testid="fleet-page" />;
  }

  const regWhy = registerBlocked(st);
  const unregWhy = unregisterBlocked(st);
  const pending = st.state === 'registering' || st.state === 'unregistering';
  const sync = recordSync(st);
  const actions = fleetActions(st.state);
  const actErrNode = actErr && <div role="alert" data-testid="fleet-error" style={errText}>{actErr}</div>;

  sections.push({
    id: 'this-instance',
    title: 'This instance',
    action: <span data-testid="fleet-state" style={stateChip(st.state)}>{st.state}</span>,
    body: (
      <div data-testid="fleet-status" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <div style={dim}>
          The fleet this instance belongs to. Membership is recorded on the fleet repository's main branch.
          {st.since && <> In this state since {when(st.since)}.</>}
        </div>
        <div style={kvGrid}>
          <span style={kvKey}>Agent id</span>
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            <span style={mono}>{st.agent_id}</span>
            <AgentIdCopy id={st.agent_id} />
          </span>
          {st.fleet_repo && (
            <>
              <span style={kvKey}>Fleet repository</span>
              <span>
                <span style={{ color: '#ddd' }}>{st.fleet_repo}</span>
                {st.fleet_url && <span style={{ ...mono, display: 'block' }}>{st.fleet_url}</span>}
              </span>
              <span style={kvKey}>Record on main</span>
              <span data-testid="fleet-record-state" style={{ color: '#ddd' }}>
                {st.record_state ?? 'pending'}
                {st.record_state === 'pending' && <span style={dim}> — waiting for someone to merge this instance's branch</span>}
              </span>
            </>
          )}
          <span style={kvKey}>External addresses</span>
          <span data-testid="fleet-external-addresses" style={mono}>
            {(st.external_addresses ?? []).length > 0 ? (st.external_addresses ?? []).join(' ') : <span style={dim}>none</span>}
          </span>
          {st.record && (
            <>
              <span style={kvKey}>This instance's record</span>
              <span>
                <span data-testid="fleet-own-record" style={{ ...mono, display: 'block' }}>
                  <span style={{ display: 'block' }}>addresses: {st.record.addresses.length > 0 ? st.record.addresses.join(' ') : '—'}</span>
                  <span style={{ display: 'block' }}>capabilities: {capabilitiesText(st.record.capabilities) || '—'}</span>
                </span>
                {sync && (
                  <span data-testid="fleet-record-sync" style={{ ...(sync.warn ? { ...dim, color: '#fbbf24' } : dim), display: 'block' }}>{sync.text}</span>
                )}
              </span>
            </>
          )}
        </div>
        {st.notice && <div data-testid="fleet-notice" style={{ ...dim, color: '#fbbf24' }}>{st.notice}</div>}
        {st.last_error && (
          <div data-testid="fleet-last-error" role="alert" style={errText}>
            Last attempt {when(st.last_attempt)} failed: <span style={mono}>{st.last_error}</span>
            {pending && <div style={dim}>Retrying on the next sync.</div>}
          </div>
        )}
        {loadErr && <div role="alert" style={errText}>{loadErr}</div>}
      </div>
    ),
  });

  sections.push({
    id: 'members',
    title: 'Members',
    hint: st.fleet_repo ? "the fleet's main" : undefined,
    body: !st.fleet_repo ? (
      <div style={dim}>This instance is not in a fleet.</div>
    ) : (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
        <div style={dim}>
          {members.length} {members.length === 1 ? 'instance' : 'instances'}. Refreshes every {POLL_MS / 1000} seconds.
        </div>
        {members.map(m => (
          <div key={m.path ?? m.agent} data-testid="fleet-member" style={{ display: 'flex', flexWrap: 'wrap', columnGap: 10, ...mono }}>
            <span style={{ color: '#ddd' }}>{m.agent}</span>
            <span>{m.state}</span>
            {m.host && <span style={{ color: '#888' }}>{m.host}</span>}
            {(m.addresses ?? []).length > 0 && <span>{(m.addresses ?? []).join(' ')}</span>}
            {capabilitiesText(m.capabilities) && <span style={{ color: '#888' }}>{capabilitiesText(m.capabilities)}</span>}
          </div>
        ))}
      </div>
    ),
  });

  if (actions.includes('join')) {
    sections.push({
      id: 'join',
      title: 'Join a fleet',
      body: (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          <div style={dim}>
            Joining pushes this instance's member record on its own branch of the fleet repository; someone with access
            to that repository accepts it by merging the branch into its main.
          </div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
            <input aria-label="Fleet repository URL" placeholder="https://… or git@…" value={url} onChange={e => setUrl(e.target.value)} style={input} />
            <input aria-label="Access token" placeholder="access token (optional)" type="password" value={token} onChange={e => setToken(e.target.value)} style={input} />
            <button type="button" data-testid="fleet-register" style={btn(busy || !!regWhy || url.trim() === '', 'primary')}
              disabled={busy || !!regWhy || url.trim() === ''} title={regWhy}
              onClick={() => act(() => api.registerFleet(url.trim(), token))}>
              Register
            </button>
          </div>
          {regWhy && <div data-testid="fleet-register-why" style={dim}>{regWhy}</div>}
          {!actions.includes('leave') && actErrNode}
        </div>
      ),
    });
  }

  if (actions.includes('leave')) {
    sections.push({
      id: 'leave',
      title: 'Leave fleet',
      danger: true,
      body: (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8, alignItems: 'flex-start' }}>
          <div style={dim}>
            Pushes a "left" record first, and unmounts the fleet repository only once that push lands.
          </div>
          <button type="button" data-testid="fleet-unregister" style={btn(busy || !!unregWhy, 'danger')}
            disabled={busy || !!unregWhy} title={unregWhy}
            onClick={() => act(() => api.unregisterFleet())}>
            Unregister
          </button>
          {unregWhy && <div data-testid="fleet-unregister-why" style={dim}>{unregWhy}</div>}
          {actErrNode}
        </div>
      ),
    });
  }

  return <SettingsPage kind="fleet" sections={sections} testid="fleet-page" />;
}

// The state as a chip on the heading, in the rail's chip geometry: green once
// registered, amber while a push is pending, neutral when standalone.
const stateChip = (state: FleetStatus['state']): React.CSSProperties => ({
  fontSize: 10.5, lineHeight: 1.7, padding: '0 7px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap',
  ...(state === 'registered'
    ? { color: '#7c9', background: '#1a231d', border: '1px solid #2a4a33' }
    : state === 'standalone'
      ? { color: '#999', background: '#1a1a1a', border: '1px solid #2a2a2a' }
      : { color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22' }),
});
const kvGrid: React.CSSProperties = {
  display: 'grid', gridTemplateColumns: 'minmax(120px, max-content) 1fr', columnGap: 18, rowGap: 6,
  fontSize: 12, alignItems: 'baseline',
};
const kvKey: React.CSSProperties = { color: '#888', fontSize: 12 };
