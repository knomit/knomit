import { useCallback, useEffect, useState } from 'react';
import { api, type FleetMember, type FleetStatus } from './api';
import { CopyIcon } from './icons';
import { SettingsPage, type Section } from './SettingsPage';

// The Fleet server page: this instance's fleet membership (F09) as
// GET /api/v1/fleet reports it: standalone, registering (a forge create whose
// publish of main is pending) or registered. Every error the server records
// (last_error) is shown verbatim with when it happened.
//
// Every fleet operation (create, enroll, join, leave, revoke, decommission,
// the root's move) is a route served on the local listener only, which a
// browser page cannot reach; the page shows the state and the members.

// Pushes are retried on the sync loop, so the state moves without a click.
const POLL_MS = 5_000;

const mono: React.CSSProperties = { fontFamily: 'ui-monospace, monospace', fontSize: 12, wordBreak: 'break-all' };
const dim: React.CSSProperties = { color: '#888', fontSize: 12 };
const errText: React.CSSProperties = { color: '#f87171', fontSize: 12, marginTop: 6 };

const copyBtn: React.CSSProperties = { background: 'none', border: 'none', padding: 2, cursor: 'pointer', display: 'flex', alignItems: 'center', flexShrink: 0 };

// AgentIdCopy copies this instance's agent id (what the master's enroll
// takes) exactly as the server reports it: the id is opaque, so nothing is
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
    return { text: 'differs from knomit.toml / this binary: restart to update it', warn: true };
  }
  if (st.record_pending_update) {
    return { text: "pending update: waiting for the fleet's master to merge this instance's branch into the fleet's main", warn: false };
  }
  return { text: 'current', warn: false };
}

// FleetPage is the Fleet server page: the membership as ruled blocks with an
// index beside them (This instance, Members).
export function FleetPage() {
  const [st, setSt] = useState<FleetStatus | null>(null);
  const [members, setMembers] = useState<FleetMember[]>([]);
  // loadErr is a failed poll.
  const [loadErr, setLoadErr] = useState('');

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

  const sections: Section[] = [];
  if (!st) {
    sections.push({
      id: 'this-instance', title: 'This instance',
      body: loadErr ? <div role="alert" style={errText}>{loadErr}</div> : <div style={dim}>Loading…</div>,
    });
    return <SettingsPage kind="fleet" sections={sections} testid="fleet-page" />;
  }

  const pending = st.state === 'registering';
  const sync = recordSync(st);

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
                {st.record_state === 'pending' && <span style={dim}> — waiting for the fleet's master to merge this instance's branch</span>}
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
