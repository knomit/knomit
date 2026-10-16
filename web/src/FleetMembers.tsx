import { Fragment, useCallback, useEffect, useState, type CSSProperties } from 'react';
import { api, type FleetEnrollment, type FleetMember, type FleetStatus } from './api';
import { FacetToolbar } from './FacetToolbar';
import { OutcomeCard } from './OutcomeCard';
import { Chip, CopyValue, Refusal } from './fleetParts';
import { STATE_ORDER, memberCounts, shortTip, stateSubline, versionText, saveTextFile } from './fleetText';
import { FLEET_POLL_MS } from './useFleet';
import { btn, card, cardLabel, confirmBox, confirmInput, fieldHint, fieldInput, fieldLabel } from './manageStyles';
import { tableWrap, th, policyFooter } from './tableStyles';
import { relativeTime } from './utils';

/** The card the Members tab shows once after New fleet: the create's or the
 *  join's landing. */
export type FleetLanding = { kind: 'created' } | { kind: 'joined'; rootFingerprint: string };

/**
 * FleetMembers is the fleet target's first tab (its Browse): who is in the
 * fleet, from GET /fleet/members (records on the fleet's main, and enrolled
 * ids). The master enrolls from the toolbar (a panel under it) and revokes
 * from a row's menu; both are served to the desktop app only, and GET
 * /fleet's `local` is the one thing that decides whether they are offered.
 */
export function FleetMembers({ fleet, landing, onDismissLanding, onChanged }: {
  fleet: FleetStatus;
  landing: FleetLanding | null;
  onDismissLanding: () => void;
  onChanged: () => void;
}) {
  const [members, setMembers] = useState<FleetMember[] | null>(null);
  const [loadErr, setLoadErr] = useState('');
  const [enrolling, setEnrolling] = useState(false);
  const [menu, setMenu] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<string | null>(null);
  const local = fleet.local === true;
  const master = fleet.master === true;
  const acts = master && local;

  const load = useCallback(() => {
    api.listFleetMembers()
      .then(ms => { setMembers(ms); setLoadErr(''); })
      .catch(e => setLoadErr(e instanceof Error ? e.message : String(e)));
  }, []);
  useEffect(() => {
    load();
    const t = setInterval(load, FLEET_POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  const changed = () => { load(); onChanged(); };
  const rows = [...(members ?? [])].sort((a, b) =>
    (STATE_ORDER[a.state] - STATE_ORDER[b.state]) || (a.agent === fleet.agent_id ? -1 : b.agent === fleet.agent_id ? 1 : 0));

  return (
    <div data-testid="fleet-members" style={body} onClick={() => { if (menu) setMenu(null); }}>
      <FacetToolbar testId="fleet-members-toolbar">
        <span style={title}>Members</span>
        <span data-testid="fleet-member-counts" style={counts}>{members ? memberCounts(members) : ''}</span>
        {acts && (
          <span style={{ marginLeft: 'auto' }}>
            <button type="button" data-testid="fleet-enroll-open" aria-pressed={enrolling} style={btn(false)}
              onClick={() => setEnrolling(e => !e)}>Enroll a member…</button>
          </span>
        )}
      </FacetToolbar>
      <div style={scroll}>
        {master && !local && (
          <div style={panel}>
            <OutcomeCard tone="ask" testid="fleet-members-desktop-only" headline="Enroll and revoke need the desktop app"
              body="This page reaches knomit over the network. Enrolling and revoking use the fleet root key, and knomit serves them only to the desktop app on this machine." />
          </div>
        )}
        {landing && (
          <div style={panel}>
            <OutcomeCard tone="good" testid="fleet-landing"
              headline={landing.kind === 'created' ? 'Fleet created' : 'Joined the fleet'}
              body={landing.kind === 'created'
                ? 'This instance is the master and its first member. Enroll a member when another instance sends you its agent id and public key.'
                : `Fleet root ${landing.rootFingerprint}. Your record is on your branch; it counts once the master's next round merges it into main.`}>
              <button type="button" style={btn(false)} onClick={onDismissLanding}>Dismiss</button>
            </OutcomeCard>
          </div>
        )}
        {acts && enrolling && (
          <EnrollPanel fleet={fleet} onClose={() => setEnrolling(false)} onEnrolled={changed}
            onRevoke={agent => { setEnrolling(false); setRevoking(agent); }} />
        )}
        {loadErr && <p role="alert" style={errorLine}>{loadErr}</p>}
        <div style={tableWrap}>
          <table style={table}>
            <thead>
              <tr>
                <th style={thCell}>Instance</th><th style={thCell}>State</th><th style={thCell}>Addresses</th>
                <th style={thCell}>Version</th><th style={thCell}>Since</th><th style={thCell}>Last refusal</th><th style={thCell} />
              </tr>
            </thead>
            <tbody>
              {rows.map(m => {
                const mine = m.agent === fleet.agent_id;
                const sub = m.state === 'revoked' ? '' : stateSubline(m);
                const canRevoke = acts && !mine && m.state !== 'revoked';
                return (
                  <Fragment key={m.path ?? m.agent}>
                    <tr data-testid="fleet-member-row" data-agent={m.agent} style={{ color: mine ? '#eee' : '#ccc' }}>
                      <td style={{ ...td, ...monoCell }}>
                        {m.agent}
                        <span style={subLine}>{m.host || '—'}{mine ? ' · this instance' : ''}</span>
                      </td>
                      <td style={td}>
                        <Chip text={m.state} tone={m.state} testid={`fleet-member-state-${m.agent}`} />
                        {sub && <span style={subLine}>{sub}</span>}
                      </td>
                      <td style={{ ...td, ...monoCell }}>
                        {(m.addresses ?? []).length ? (m.addresses ?? []).join(' ') : <span style={{ color: '#555' }}>none</span>}
                      </td>
                      <td style={{ ...td, ...monoCell }}>{versionText(m.capabilities) || '—'}</td>
                      <td style={{ ...td, whiteSpace: 'nowrap' }}>{m.since ? <span title={m.since}>{relativeTime(m.since)}</span> : '—'}</td>
                      <td style={td}>
                        {m.last_refusal ? (
                          <div data-testid={`fleet-member-refusal-${m.agent}`} style={refusal}>
                            {m.last_refusal.branch} @ {shortTip(m.last_refusal.tip)}
                            <span style={refusalSub}>{m.last_refusal.reason}</span>
                            {/* The refused tip's author is a CLAIM: the tip was
                                refused, so nothing verified it (#527 F2). */}
                            {m.last_refusal.agent && <span style={refusalSub}>claimed by {m.last_refusal.agent}</span>}
                            <span style={refusalSub}>{relativeTime(m.last_refusal.at)} · left behind until the branch moves</span>
                          </div>
                        ) : <span style={{ color: '#444' }}>—</span>}
                      </td>
                      <td style={{ ...td, position: 'relative', textAlign: 'right', whiteSpace: 'nowrap' }}>
                        {canRevoke && (
                          <button type="button" data-testid={`fleet-member-menu-${m.agent}`} aria-label={`Actions for ${m.agent}`}
                            aria-haspopup="menu" style={menuBtn}
                            onClick={e => { e.stopPropagation(); setMenu(menu === m.agent ? null : m.agent); }}>⋯</button>
                        )}
                        {canRevoke && menu === m.agent && (
                          <div role="menu" style={menuBox}>
                            <button type="button" role="menuitem" data-testid="fleet-member-revoke" style={menuItem}
                              onClick={() => { setMenu(null); setRevoking(m.agent); }}>Revoke certificate…</button>
                          </div>
                        )}
                      </td>
                    </tr>
                    {revoking === m.agent && (
                      <tr><td colSpan={7} style={{ padding: '0 0 10px' }}>
                        <RevokeConfirm member={m} onCancel={() => setRevoking(null)} onRevoked={() => { setRevoking(null); changed(); }} />
                      </td></tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
          {/* A refused enroll's "Revoke <id>…" for an id with no row yet. */}
          {revoking && !rows.some(m => m.agent === revoking) && (
            <RevokeConfirm member={{ agent: revoking, state: 'enrolled' }} onCancel={() => setRevoking(null)}
              onRevoked={() => { setRevoking(null); changed(); }} />
          )}
        </div>
      </div>
      <p style={policyFooter}>
        Member records on the fleet's main; revoked is read from the CRL on the same main.{' '}
        {master ? 'Pushes from a left member are accepted until it is revoked.' : "Only the fleet's master enrolls, revokes and merges."}{' '}
        Refreshes every 5 seconds.
      </p>
    </div>
  );
}

/** EnrollPanel: the master enrolls a joiner (POST /fleet/enroll) and hands
 *  over the bundle, which is copied or saved and never shown. */
function EnrollPanel({ fleet, onClose, onEnrolled, onRevoke }: {
  fleet: FleetStatus; onClose: () => void; onEnrolled: () => void; onRevoke: (agent: string) => void;
}) {
  const [agent, setAgent] = useState('');
  const [key, setKey] = useState('');
  const [address, setAddress] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | null>(null);
  const [result, setResult] = useState<FleetEnrollment | null>(null);
  const [copied, setCopied] = useState(false);
  const [saved, setSaved] = useState('');
  // Only a hub with several external addresses asks: the bundle carries one
  // URL. A refusal that lists the choices (address_required/unknown) asks too.
  const refusalChoices = (err as { choices?: string[] } | null)?.choices ?? [];
  const choices = fleet.hosting === 'none' && fleet.host.external_addresses.length > 1
    ? fleet.host.external_addresses : refusalChoices;

  const enroll = async () => {
    setBusy(true); setErr(null);
    try {
      const r = await api.enrollFleetMember(agent.trim(), key.trim(), address || undefined);
      setResult(r);
      onEnrolled();
    } catch (e) {
      setErr(e instanceof Error ? e : new Error(String(e)));
    }
    setBusy(false);
  };

  if (result) {
    const fileName = `${result.agent_id}.fleet-bundle`;
    return (
      <div data-testid="fleet-enroll-result" style={panel}>
        <OutcomeCard tone={result.published ? 'good' : 'bad'}
          headline={result.published ? `Enrolled ${result.agent_id}` : `Enrolled ${result.agent_id}; not on main yet`}
          body={result.published
            ? `Serial ${result.serial}. Main holds its certificate: the joiner can join now.`
            : `Serial ${result.serial}. The certificate is on this instance's branch; the sync loop publishes it again. A join is refused until main holds it.`}
          detail={result.published ? undefined : result.publish_error} detailLabel="the last attempt reported">
          <div style={{ ...cardLabel, marginTop: 8 }}>Send the joiner</div>
          <div style={btns}>
            <button type="button" data-testid="fleet-copy-bundle" style={btn(false, 'primary')}
              onClick={() => {
                navigator.clipboard.writeText(result.bundle)
                  .then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); })
                  .catch(() => { /* clipboard unavailable */ });
              }}>{copied ? 'Copied' : 'Copy bundle'}</button>
            <button type="button" data-testid="fleet-save-bundle" style={btn(!!saved)} disabled={!!saved}
              onClick={() => { saveTextFile(fileName, result.bundle); setSaved(fileName); }}>
              {saved ? `Saved ${saved}` : 'Save bundle…'}
            </button>
          </div>
          <div style={fieldHint}>
            The bundle holds the fleet's URL (<span data-testid="fleet-enroll-url" style={monoInline}>{result.fleet_url}</span>) and this joiner's certificate. It is for this one instance: a bundle for another agent is refused.
          </div>
          <div style={{ display: 'flex', gap: 18, alignItems: 'baseline', marginTop: 10, flexWrap: 'wrap', fontSize: 12 }}>
            <span style={{ color: '#888' }}>Root fingerprint</span>
            <CopyValue text={result.root_fingerprint} label="Copy fingerprint" testid="fleet-enroll-fingerprint" />
          </div>
          <div style={fieldHint}>Give the joiner this fingerprint by a different channel than the bundle; their Join step shows it for checking before anything is installed.</div>
        </OutcomeCard>
        <div style={{ ...btns, marginTop: 12 }}>
          <button type="button" style={btn(false)} onClick={() => { setResult(null); setAgent(''); setKey(''); setSaved(''); }}>Enroll another</button>
          <button type="button" style={btn(false)} onClick={onClose}>Done</button>
        </div>
      </div>
    );
  }

  const disabled = busy || !agent.trim() || !key.trim() || (choices.length > 1 && !address);
  return (
    <div data-testid="fleet-enroll-panel" style={panel}>
      <div style={cardLabel}>Enroll a member</div>
      <div style={{ color: '#888', fontSize: 12 }}>Both come from the joiner's New fleet step on their machine (Join a fleet → Start).</div>
      <label style={fieldLabel} htmlFor="fleet-enroll-agent">Agent id</label>
      <input id="fleet-enroll-agent" data-testid="fleet-enroll-agent" style={fieldInput} value={agent} placeholder="as the joiner's Start step shows it"
        autoCapitalize="off" autoCorrect="off" spellCheck={false} onChange={e => setAgent(e.target.value)} />
      <label style={fieldLabel} htmlFor="fleet-enroll-key">Public key</label>
      <input id="fleet-enroll-key" data-testid="fleet-enroll-key" style={{ ...fieldInput, fontFamily: 'var(--k-font-mono)' }} value={key} placeholder="ssh-ed25519 AAAA…"
        autoCapitalize="off" autoCorrect="off" spellCheck={false} onChange={e => setKey(e.target.value)} />
      {choices.length > 1 && (
        <>
          <label style={fieldLabel} htmlFor="fleet-enroll-address">Address in the bundle</label>
          <select id="fleet-enroll-address" data-testid="fleet-enroll-address" style={fieldInput} value={address} onChange={e => setAddress(e.target.value)}>
            <option value="">Choose the address this joiner reaches…</option>
            {choices.map(a => <option key={a} value={a}>{a}</option>)}
          </select>
          <div style={fieldHint}>This instance answers at every external address; a bundle names one. Pick the one the joiner can reach.</div>
        </>
      )}
      <Refusal err={err} note="Nothing was issued." testid="fleet-enroll-error" />
      {(err as { code?: string } | null)?.code === 'enrolled' && (
        <div style={{ ...btns, marginTop: 8 }}>
          <button type="button" data-testid="fleet-enroll-revoke" style={btn(false, 'danger')} onClick={() => onRevoke(agent.trim())}>Revoke {agent.trim()}…</button>
        </div>
      )}
      <div style={{ ...btns, marginTop: 12 }}>
        <button type="button" data-testid="fleet-enroll-submit" style={btn(disabled, 'primary')} disabled={disabled} onClick={() => { void enroll(); }}>
          {busy ? 'Enrolling…' : err ? 'Try again' : 'Enroll'}
        </button>
        <button type="button" style={btn(busy)} disabled={busy} onClick={onClose}>Cancel</button>
      </div>
    </div>
  );
}

/** RevokeConfirm: the master revokes a member's certificate (POST
 *  /fleet/revoke). A same-key rejoin is supported: the master enrolls the
 *  same key again once this leaf is on the CRL. */
function RevokeConfirm({ member, onCancel, onRevoked }: { member: Pick<FleetMember, 'agent' | 'host' | 'state'>; onCancel: () => void; onRevoked: () => void }) {
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | null>(null);
  const revoke = async () => {
    setBusy(true); setErr(null);
    try {
      await api.revokeFleetMember(member.agent, reason.trim() || undefined);
      onRevoked();
    } catch (e) {
      setErr(e instanceof Error ? e : new Error(String(e)));
      setBusy(false);
    }
  };
  return (
    <div data-testid="fleet-revoke-confirm" style={{ ...confirmBox, marginTop: 4 }}>
      <div style={{ fontSize: 13, marginBottom: 8 }}>
        Revoke {member.agent}'s certificate{member.host ? ` (${member.host})` : ''}? Its serial goes on the CRL on main; every member refuses its pushes from the next sync. To readmit it later, enroll it again with the same key.
      </div>
      <label style={{ ...fieldLabel, marginTop: 0 }} htmlFor="fleet-revoke-reason">Reason (optional, kept with the revocation)</label>
      <input id="fleet-revoke-reason" data-testid="fleet-revoke-reason" style={confirmInput} value={reason} placeholder="e.g. machine reimaged"
        onChange={e => setReason(e.target.value)} />
      <Refusal err={err} testid="fleet-revoke-error" />
      <div style={{ ...btns, marginTop: 10 }}>
        <button type="button" data-testid="fleet-revoke-submit" style={btn(busy, 'danger')} disabled={busy} onClick={() => { void revoke(); }}>{busy ? 'Revoking…' : 'Revoke'}</button>
        <button type="button" style={btn(busy)} disabled={busy} onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}

const body: CSSProperties = { flex: '1 1 0', minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const scroll: CSSProperties = { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflowY: 'auto' };
const title: CSSProperties = { color: '#ddd', fontWeight: 500, fontSize: 12 };
const counts: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#888', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const panel: CSSProperties = { ...card, margin: '12px 12px 0', padding: '12px 14px', maxWidth: 760, flexShrink: 0, boxSizing: 'border-box' };
const btns: CSSProperties = { display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' };
const errorLine: CSSProperties = { color: '#f87171', fontSize: 12, margin: '6px 12px 0' };
const table: CSSProperties = { width: '100%', borderCollapse: 'collapse', fontSize: 12, minWidth: 640 };
const thCell: CSSProperties = {
  ...th, padding: '8px 10px 6px 0', textAlign: 'left', fontWeight: 500, color: '#555',
  textTransform: 'uppercase', letterSpacing: 1, fontSize: 9.5, whiteSpace: 'nowrap',
};
const td: CSSProperties = { padding: '7px 10px 7px 0', borderBottom: '1px solid #1a1a1a', verticalAlign: 'top' };
const monoCell: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11.5 };
const monoInline: CSSProperties = { fontFamily: 'var(--k-font-mono)' };
const subLine: CSSProperties = { display: 'block', color: '#666', fontSize: 11, marginTop: 2, fontFamily: 'var(--k-font-body)' };
const refusal: CSSProperties = { color: '#e2c07a', fontSize: 11.5 };
const refusalSub: CSSProperties = { display: 'block', color: '#8a7440', fontSize: 11, marginTop: 2 };
const menuBtn: CSSProperties = { background: 'none', border: 0, color: '#888', cursor: 'pointer', padding: '0 6px', fontSize: 14, lineHeight: 1, borderRadius: 3 };
const menuBox: CSSProperties = {
  position: 'absolute', right: 0, top: '100%', zIndex: 5, background: '#161616', border: '1px solid #333',
  borderRadius: 5, padding: 4, minWidth: 160, boxShadow: '0 6px 18px rgba(0,0,0,.5)', textAlign: 'left',
};
const menuItem: CSSProperties = {
  display: 'block', width: '100%', textAlign: 'left', background: 'none', border: 0, padding: '6px 10px',
  fontSize: 12.5, color: '#e8a0a0', borderRadius: 3, cursor: 'pointer',
};
