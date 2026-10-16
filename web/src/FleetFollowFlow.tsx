import { useEffect, useState } from 'react';
import { api, type ProbeResult } from './api';
import { GlobeIcon } from './icons';
import { btn } from './manageStyles';
import { OutcomeCard } from './OutcomeCard';
import { Refusal } from './fleetParts';
import { hostOf } from './wizardState';
import { noMouseFocus } from './utils';
import {
  actions, block, blockHead, blockHint, blockTitle, column, crumb, crumbLink, crumbSep, grid, input, label,
  pageHead, rail, railItem, railLabel, railMark, railNote,
} from './connectStyles';

const FLEET_ACCENT = '#8ad6c8';

/**
 * FleetFollowFlow is the fleet's "Point at the new host": the Connect flow's
 * page (crumb, head, Origin then Review, the progress rail) with only what
 * following a new host needs. The URL is the new master's fleet URL; the
 * check is the repo wizard's probe (POST /repos:probe-origin, a read); the
 * change is POST /fleet/follow, which refuses a URL that is not an instance
 * of this fleet. No credential is asked: an instance of this fleet takes this
 * instance's certificate.
 */
export function FleetFollowFlow({ fleetRepo, onCancel, onDone, onBusyChange }: {
  fleetRepo: string;
  onCancel: () => void;
  onDone: () => void;
  onBusyChange?: (busy: boolean) => void;
}) {
  const [url, setUrl] = useState('');
  const [step, setStep] = useState<1 | 2>(1);
  const [testing, setTesting] = useState(false);
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const [probeErr, setProbeErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | null>(null);
  useEffect(() => {
    onBusyChange?.(busy);
    return () => { onBusyChange?.(false); };
  }, [busy, onBusyChange]);

  const trimmed = url.trim();
  const host = hostOf(trimmed);
  const test = async () => {
    setTesting(true); setProbe(null); setProbeErr(''); setErr(null);
    try {
      const r = await api.probeOrigin({ url: trimmed });
      setProbe(r);
      if (r.reachable && !r.auth_required) setStep(2);
    } catch (e) { setProbeErr(e instanceof Error ? e.message : String(e)); }
    setTesting(false);
  };
  const follow = async () => {
    setBusy(true); setErr(null);
    try {
      await api.followFleetHost(trimmed);
      setBusy(false);
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e : new Error(String(e)));
      setBusy(false);
    }
  };
  const refusedProbe = probe && (!probe.reachable || probe.auth_required);

  return (
    <div data-testid="fleet-follow-flow">
      <nav style={crumb} aria-label="Breadcrumb">
        <button type="button" data-testid="fleet-follow-back" className="k-bare" onMouseDown={noMouseFocus}
          style={crumbLink(busy)} disabled={busy} onClick={onCancel}>{fleetRepo}</button>
        <span style={crumbSep} aria-hidden="true">/</span>
        <span>Point at the new host</span>
      </nav>
      <div style={pageHead}>
        <span style={iconBox}><GlobeIcon color={FLEET_ACCENT} size={16} /></span>
        <div style={{ minWidth: 0 }}>
          <h3 style={{ margin: 0, fontSize: 16 }}>Point at the new host</h3>
          <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>Connect the fleet repository to the new host after a master switch</div>
        </div>
      </div>

      <div style={grid}>
        <div style={column}>
          {step === 1 && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Origin</h4>
                <span style={blockHint}>where the fleet is hosted now</span>
              </div>
              <div>
                <label style={label} htmlFor="fleet-follow-url">Fleet URL (from the new master's Settings → Hosting)</label>
                <input id="fleet-follow-url" data-testid="fleet-follow-url" style={input} value={url} disabled={testing}
                  placeholder="https://new-master.example:19279/git/fleet" autoCapitalize="off" autoCorrect="off" spellCheck={false}
                  onChange={e => { setUrl(e.target.value); setProbe(null); setProbeErr(''); }} />
              </div>
              {(probeErr || refusedProbe) && (
                <OutcomeCard tone="bad" testid="fleet-follow-unreachable" headline={host ? `Couldn't reach ${host}` : 'Could not reach that URL'}
                  url={trimmed} detail={probeErr || probe?.detail || undefined} />
              )}
              <div style={actions}>
                <button type="button" data-testid="fleet-follow-test" style={btn(!trimmed || testing, 'primary')} disabled={!trimmed || testing}
                  onClick={() => { void test(); }}>{testing ? 'Testing…' : 'Test connection →'}</button>
                <button type="button" style={btn(testing)} disabled={testing} onClick={onCancel}>Cancel</button>
              </div>
            </section>
          )}
          {step === 2 && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Review</h4>
                <span style={blockHint}>what following the new host changes</span>
              </div>
              <OutcomeCard tone="good" testid="fleet-follow-reached" headline={`Reached ${host || trimmed}`} url={trimmed} />
              <ol style={steps}>
                <li>This copy of {fleetRepo} follows the fleet from {trimmed}: it fetches main there and pushes its own branch there.</li>
                <li>knomit checks first that {host || 'the URL'} is an instance of this fleet; anything else is refused and nothing changes.</li>
              </ol>
              <Refusal err={err} note="Nothing changed." testid="fleet-follow-error" />
              <div style={actions}>
                <button type="button" data-testid="fleet-follow-submit" style={btn(busy, 'primary')} disabled={busy}
                  onClick={() => { void follow(); }}>{busy ? 'Connecting…' : err ? 'Try again' : 'Connect'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => { setStep(1); setErr(null); }}>Back</button>
              </div>
            </section>
          )}
        </div>
        <nav style={rail} aria-label="Progress">
          <div style={railLabel}>Progress</div>
          {([[1, 'Origin'], [2, 'Review']] as const).map(([n, lbl]) => {
            const state = step === n ? 'active' : step > n ? 'done' : 'todo';
            return (
              <div key={n} data-testid={`fleet-follow-step-${n}`} aria-current={state === 'active' ? 'step' : undefined} style={railItem(state)}>
                <span style={railMark(state)}>{state === 'done' ? '✓' : n}</span>
                <span>{lbl}</span>
              </div>
            );
          })}
          <p style={railNote}>This copy still follows the old host until the last step runs.</p>
        </nav>
      </div>
    </div>
  );
}

const iconBox: React.CSSProperties = {
  width: 30, height: 30, borderRadius: 7, flexShrink: 0, background: '#121f1d', border: '1px solid #24453f',
  display: 'flex', alignItems: 'center', justifyContent: 'center',
};
const steps: React.CSSProperties = { margin: '2px 0 0 18px', padding: 0, fontSize: 12.5, color: '#ccc', lineHeight: 1.65 };
