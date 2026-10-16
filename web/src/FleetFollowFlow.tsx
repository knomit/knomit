import { useEffect, useState } from 'react';
import { api, type ProbeResult } from './api';
import { GlobeIcon } from './icons';
import { btn } from './manageStyles';
import { OutcomeCard } from './OutcomeCard';
import { CredentialFields, Refusal } from './fleetParts';
import { hostPortOf } from './fleetText';
import { credentialFor, type FleetAuthMethod } from './fleetWizardState';
import { noMouseFocus } from './utils';
import {
  actions, block, blockHead, blockHint, blockTitle, column, crumb, crumbLink, crumbSep, grid, input, label,
  pageHead, rail, railItem, railLabel, railMark, railNote,
} from './connectStyles';

const FLEET_ACCENT = '#8ad6c8';

/** What the flow is for. follow: Point at the new host (a copy with an
 *  origin, or the hub after a master switch). access: Update access, a forge
 *  copy's credential (and, if wanted, another forge URL). move: Move the fleet
 *  to a forge, the hosting master's. All three end in POST /fleet/follow. */
export type FleetFollowMode = 'follow' | 'access' | 'move';

type Step = 'origin' | 'access' | 'review';

const TITLE: Record<FleetFollowMode, string> = {
  follow: 'Point at the new host',
  access: 'Update access',
  move: 'Move the fleet to a forge',
};
const SUBTITLE: Record<FleetFollowMode, string> = {
  follow: 'Connect the fleet repository to where the fleet is hosted now',
  access: "Change this instance's credential for the forge the fleet lives on",
  move: 'Host the fleet repository on a forge; this instance stays the master',
};

/**
 * FleetFollowFlow is the fleet's origin flow: the Connect flow's page
 * (crumb, head, Origin, Access, Review, the progress rail) with only what the
 * fleet repository needs. The check is the repo wizard's probe (POST
 * /repos:probe-origin, a read). An instance of this fleet (fleet_peer) takes
 * this instance's certificate, so it skips Access; a forge gets the Access
 * step's credential fields (CredentialFields, the repo step's). The change is
 * POST /fleet/follow, which refuses a URL that is not this fleet's.
 */
export function FleetFollowFlow({ fleetRepo, mode = 'follow', initialUrl = '', initialMethod = '', hostRecord, onCancel, onDone, onBusyChange }: {
  fleetRepo: string;
  mode?: FleetFollowMode;
  /** Prefilled: the stored forge URL (access), or where main says the fleet
   *  is hosted now (follow, when this copy follows an old host). */
  initialUrl?: string;
  initialMethod?: FleetAuthMethod;
  /** The host record's URLs, for the Origin step's hint. */
  hostRecord?: string[];
  onCancel: () => void;
  onDone: () => void;
  onBusyChange?: (busy: boolean) => void;
}) {
  const [url, setUrl] = useState(initialUrl);
  const [step, setStep] = useState<Step>('origin');
  const [testing, setTesting] = useState(false);
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const [probeErr, setProbeErr] = useState('');
  const [method, setMethod] = useState<FleetAuthMethod>(initialMethod);
  const [user, setUser] = useState('');
  const [secret, setSecret] = useState('');
  const [checked, setChecked] = useState(false);
  const [checkErr, setCheckErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | null>(null);
  useEffect(() => {
    onBusyChange?.(busy);
    return () => { onBusyChange?.(false); };
  }, [busy, onBusyChange]);

  const trimmed = url.trim();
  const host = hostPortOf(trimmed);
  const peer = probe?.fleet_peer === true;
  const cred = peer ? {} : credentialFor(trimmed, method, user, secret);
  const steps: Step[] = peer ? ['origin', 'review'] : ['origin', 'access', 'review'];

  const test = async () => {
    setTesting(true); setProbe(null); setProbeErr(''); setErr(null); setChecked(false); setCheckErr('');
    try {
      const r = await api.probeOrigin({ url: trimmed });
      setProbe(r);
      if (r.reachable && r.fleet_peer && !r.auth_required) setStep(mode === 'move' ? 'origin' : 'review');
      else if (r.reachable && !r.fleet_peer) setStep('access');
    } catch (e) { setProbeErr(e instanceof Error ? e.message : String(e)); }
    setTesting(false);
  };
  const checkAccess = async () => {
    setTesting(true); setChecked(false); setCheckErr('');
    try {
      const r = await api.probeOrigin({ url: trimmed, ...cred });
      if (r.reachable && !r.auth_required) setChecked(true);
      else setCheckErr(r.detail || (r.auth_required ? 'the forge refused these credentials' : 'the forge could not be reached'));
    } catch (e) { setCheckErr(e instanceof Error ? e.message : String(e)); }
    setTesting(false);
  };
  const follow = async () => {
    setBusy(true); setErr(null);
    try {
      await api.followFleetHost(trimmed, cred);
      setBusy(false);
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e : new Error(String(e)));
      setBusy(false);
    }
  };
  const unreachable = probe && !probe.reachable;
  const peerForMove = mode === 'move' && peer;
  const asked = !!probe?.auth_required;
  const fieldLabelText = mode === 'follow'
    ? "Fleet URL (from the new master's Settings → Hosting)"
    : 'Forge repository URL';

  return (
    <div data-testid="fleet-follow-flow" data-mode={mode}>
      <nav style={crumb} aria-label="Breadcrumb">
        <button type="button" data-testid="fleet-follow-back" className="k-bare" onMouseDown={noMouseFocus}
          style={crumbLink(busy)} disabled={busy} onClick={onCancel}>{fleetRepo}</button>
        <span style={crumbSep} aria-hidden="true">/</span>
        <span>{TITLE[mode]}</span>
      </nav>
      <div style={pageHead}>
        <span style={iconBox}><GlobeIcon color={FLEET_ACCENT} size={16} /></span>
        <div style={{ minWidth: 0 }}>
          <h3 style={{ margin: 0, fontSize: 16 }}>{TITLE[mode]}</h3>
          <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>{SUBTITLE[mode]}</div>
        </div>
      </div>

      <div style={grid}>
        <div style={column}>
          {step === 'origin' && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Origin</h4>
                <span style={blockHint}>{mode === 'move' ? 'the forge repository the fleet moves to' : 'where the fleet is hosted now'}</span>
              </div>
              <div>
                <label style={label} htmlFor="fleet-follow-url">{fieldLabelText}</label>
                <input id="fleet-follow-url" data-testid="fleet-follow-url" style={input} value={url} disabled={testing}
                  placeholder={mode === 'follow' ? 'https://new-master.example:19279/git/fleet' : 'https://github.com/acme/fleet'}
                  autoCapitalize="off" autoCorrect="off" spellCheck={false}
                  onChange={e => { setUrl(e.target.value); setProbe(null); setProbeErr(''); }} />
                {mode === 'follow' && hostRecord && hostRecord.length > 0 && (
                  <div data-testid="fleet-follow-host-record" style={hintText}>The fleet's main says it is hosted at {hostRecord.join(', ')}.</div>
                )}
                {mode === 'move' && (
                  <div style={hintText}>An empty repository on the forge, or one this fleet already lives on. The next step asks for this instance's credential.</div>
                )}
              </div>
              {(probeErr || unreachable) && (
                <OutcomeCard tone="bad" testid="fleet-follow-unreachable" headline={host ? `Couldn't reach ${host}` : 'Could not reach that URL'}
                  url={trimmed} detail={probeErr || probe?.detail || undefined} />
              )}
              {peerForMove && (
                <OutcomeCard tone="bad" testid="fleet-follow-peer-not-forge" headline={`${host || trimmed} is an instance of this fleet, not a forge`}
                  url={trimmed} body="To make another instance the host, export the root key there and choose Host the fleet here on it. Moving to a forge takes a forge repository's URL." />
              )}
              <div style={actions}>
                <button type="button" data-testid="fleet-follow-test" style={btn(!trimmed || testing, 'primary')} disabled={!trimmed || testing}
                  onClick={() => { void test(); }}>{testing ? 'Testing…' : 'Test connection →'}</button>
                <button type="button" style={btn(testing)} disabled={testing} onClick={onCancel}>Cancel</button>
              </div>
            </section>
          )}
          {step === 'access' && (
            <section style={block} data-testid="fleet-follow-access">
              <div style={blockHead}>
                <h4 style={blockTitle}>Access</h4>
                <span style={blockHint}>this instance's own credential for the forge</span>
              </div>
              {checked ? (
                <OutcomeCard tone="good" testid="fleet-follow-access-ok" headline={`Reached ${host || trimmed} with these credentials`} url={trimmed} />
              ) : asked ? (
                <OutcomeCard tone="ask" testid="fleet-follow-access-ask" headline={`${host || 'The forge'} wants credentials`} url={trimmed}
                  body="Reading the fleet repository was refused anonymously. Give this instance's own token for the forge: it pushes its branch there." />
              ) : (
                <OutcomeCard tone="good" testid="fleet-follow-reached" headline={`Reached ${host || trimmed}`} url={trimmed}
                  body="A forge: this instance pushes its own branch there, so it needs a credential with write access unless the forge lets anyone push." />
              )}
              {checkErr && (
                <div style={{ marginTop: 12 }}>
                  <OutcomeCard tone="bad" testid="fleet-follow-access-refused" headline={`${host || 'The forge'} refused those credentials`} url={trimmed} detail={checkErr} />
                </div>
              )}
              <CredentialFields url={trimmed} method={method} user={user} token={secret} idPrefix="fleet-follow" disabled={testing}
                onMethod={m => { setMethod(m); setChecked(false); }} onUser={u => { setUser(u); setChecked(false); }} onToken={t => { setSecret(t); setChecked(false); }}
                hint={mode === 'access' && initialMethod
                  ? `Stored now: ${initialMethod}. A new token replaces it; "none" makes this copy anonymous.`
                  : "The credential is this instance's own and stays in its credential store."} />
              <div style={actions}>
                <button type="button" data-testid="fleet-follow-check" style={btn(testing)} disabled={testing}
                  onClick={() => { void checkAccess(); }}>{testing ? 'Checking…' : 'Check access'}</button>
                <button type="button" data-testid="fleet-follow-next" style={btn(testing || (asked && !checked), 'primary')} disabled={testing || (asked && !checked)}
                  onClick={() => setStep('review')}>Next →</button>
                <button type="button" style={btn(testing)} disabled={testing} onClick={() => setStep('origin')}>Back</button>
              </div>
            </section>
          )}
          {step === 'review' && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Review</h4>
                <span style={blockHint}>{mode === 'move' ? 'what moving the fleet changes' : 'what this changes'}</span>
              </div>
              {peer && <OutcomeCard tone="good" testid="fleet-follow-reached" headline={`Reached ${host || trimmed}`} url={trimmed} />}
              <ol style={stepsList} data-testid="fleet-follow-will">
                {mode === 'move' ? (
                  <>
                    <li>knomit pushes this copy's main and its own branch to {trimmed} (the forge's main must be empty, or behind this copy's), then this copy syncs from there.</li>
                    <li>main records {trimmed} as where the fleet is hosted, and every bundle you issue from now on names it.</li>
                    <li data-testid="fleet-move-members">Every member's pushes to this instance are refused from then on, until it points at {trimmed} with its own credential for the forge (its Fleet → Settings → Point at the new host…). Its unmerged work is not lost: it pushes its own branch again on its first sync there.</li>
                    <li>This instance stays the master: each round it fetches every member's branch from the forge, judges and merges it, and publishes main.</li>
                  </>
                ) : peer ? (
                  <>
                    <li>This copy of {fleetRepo} follows the fleet from {trimmed}: it fetches main there and pushes its own branch there.</li>
                    <li>knomit checks first that {host || 'the URL'} is an instance of this fleet; anything else is refused and nothing changes.</li>
                  </>
                ) : (
                  <>
                    <li>This copy of {fleetRepo} follows the fleet from {trimmed} with this instance's own credential: it fetches main there and pushes its own branch there.</li>
                    <li>knomit checks first that main on {host || 'the forge'} carries this fleet's root; anything else is refused and nothing changes.</li>
                  </>
                )}
              </ol>
              <Refusal err={err} note="Nothing changed." testid="fleet-follow-error" />
              <div style={actions}>
                <button type="button" data-testid="fleet-follow-submit" style={btn(busy, 'primary')} disabled={busy}
                  onClick={() => { void follow(); }}>{busy ? (mode === 'move' ? 'Moving…' : 'Connecting…') : err ? 'Try again' : mode === 'move' ? 'Move the fleet' : 'Connect'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => { setStep(peer ? 'origin' : 'access'); setErr(null); }}>Back</button>
              </div>
            </section>
          )}
        </div>
        <nav style={rail} aria-label="Progress">
          <div style={railLabel}>Progress</div>
          {steps.map((s, i) => {
            const at = steps.indexOf(step);
            const state = i === at ? 'active' : i < at ? 'done' : 'todo';
            return (
              <div key={s} data-testid={`fleet-follow-step-${s}`} aria-current={state === 'active' ? 'step' : undefined} style={railItem(state)}>
                <span style={railMark(state)}>{state === 'done' ? '✓' : i + 1}</span>
                <span>{s === 'origin' ? 'Origin' : s === 'access' ? 'Access' : 'Review'}</span>
              </div>
            );
          })}
          <p style={railNote}>{mode === 'move' ? 'The fleet stays hosted here until the last step runs.' : 'This copy keeps its current origin until the last step runs.'}</p>
        </nav>
      </div>
    </div>
  );
}

const iconBox: React.CSSProperties = {
  width: 30, height: 30, borderRadius: 7, flexShrink: 0, background: '#121f1d', border: '1px solid #24453f',
  display: 'flex', alignItems: 'center', justifyContent: 'center',
};
const stepsList: React.CSSProperties = { margin: '2px 0 0 18px', padding: 0, fontSize: 12.5, color: '#ccc', lineHeight: 1.65 };
const hintText: React.CSSProperties = { fontSize: 11.5, color: '#888', marginTop: 6, lineHeight: 1.5 };
