import { useReducer, useRef, useState, type CSSProperties } from 'react';
import { api, type FleetStatus } from './api';
import { FacetColumns } from './FacetColumns';
import { IndexHeading } from './IndexColumn';
import { StepRail } from './WizardStepRail';
import { SegmentedChoice } from './SegmentedChoice';
import { OutcomeCard } from './OutcomeCard';
import { CopyValue, Kv, Refusal } from './fleetParts';
import type { FleetLanding } from './FleetMembers';
import {
  fleetWizardReducer, initialFleetWizardState, stepsFor, stepSummary, FLEET_STEP_LABEL,
  accessURL, accessRequired, fleetAuth, type FleetAuthMethod, type FleetStepId, type FleetWizardState,
} from './fleetWizardState';
import { bundleHost, checkRefusalCard, joinRefusalNote, readTextFile } from './fleetText';
import { hostOf } from './wizardState';
import {
  btn, card, cardLabel, fieldCaveat, fieldHint, fieldInput, fieldLabel, segDisclosure, wizardBody, wizardFooter,
} from './manageStyles';

/**
 * FleetWizard is the New fleet draft: the create-repo wizard's paradigm (a
 * step rail in the index column, one step at a time, Back/Next, Review with
 * "What will happen", refusals under the primary button) with only the steps
 * and fields creating or joining a fleet needs. Create: Start · Hosting ·
 * [Access] · Review. Join: Start · Bundle · [Access] · Review. The joiner
 * types nothing: the bundle carries the fleet's URL, and it is sent, never
 * shown.
 */
export function FleetWizard({ fleet, onDone, onCancel }: {
  /** GET /fleet: this instance's key, listener and would_serve. */
  fleet: FleetStatus | null;
  /** The create or the join succeeded; the landing card to show. */
  onDone: (landing: FleetLanding) => void;
  onCancel: () => void;
}) {
  const [s, dispatch] = useReducer(fleetWizardReducer, initialFleetWizardState);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | null>(null);
  // A probe or check in flight, and its refusal, scoped to its step.
  const [checking, setChecking] = useState(false);
  const [checkErr, setCheckErr] = useState<Error | null>(null);
  const steps = stepsFor(s);
  const active = steps.indexOf(s.step);
  const next = steps[active + 1] as FleetStepId | undefined;
  const goto = (step: FleetStepId) => { setCheckErr(null); dispatch({ type: 'GOTO', step }); };

  const asError = (e: unknown) => (e instanceof Error ? e : new Error(String(e)));

  const probeForge = async () => {
    setChecking(true); setCheckErr(null);
    try {
      const r = await api.probeOrigin({ url: s.forgeUrl.trim() });
      // The probe's four outcomes: only an unreachable remote stops here; one
      // that wants credentials is Access's question.
      if (!r.reachable && !r.auth_required) throw new Error(r.detail || 'the remote did not answer');
      dispatch({ type: 'PROBED', probe: r });
    } catch (e) { setCheckErr(asError(e)); }
    setChecking(false);
  };
  const checkBundle = async () => {
    setChecking(true); setCheckErr(null);
    try {
      dispatch({ type: 'CHECKED', check: await api.checkFleetJoin(s.bundle) });
    } catch (e) { setCheckErr(asError(e)); }
    setChecking(false);
  };
  // Access: the check again, WITH the credential typed.
  const checkAccess = async () => {
    setChecking(true); setCheckErr(null);
    const auth = fleetAuth(s);
    try {
      if (s.choice === 'create') {
        const r = await api.probeOrigin({ url: s.forgeUrl.trim(), ...auth });
        if (!r.reachable || r.auth_required) throw new Error(r.detail || 'the remote refused the credential');
      } else {
        const r = await api.checkFleetJoin(s.bundle, auth.auth_method, auth.auth_token);
        if (r.auth_required) throw new Error(r.detail || 'the forge refused the credential');
      }
      dispatch({ type: 'ACCESS_CHECKED' });
    } catch (e) { setCheckErr(asError(e)); }
    setChecking(false);
  };
  const primary = async () => {
    setBusy(true); setErr(null);
    try {
      if (s.choice === 'create') {
        const auth = fleetAuth(s);
        await api.createFleet(s.hosting === 'forge' ? { url: s.forgeUrl.trim(), ...auth } : undefined);
        onDone({ kind: 'created' });
      } else {
        const auth = s.check?.hosting === 'forge' ? fleetAuth(s) : {};
        const st = await api.joinFleet(s.bundle, auth.auth_method, auth.auth_token);
        onDone({ kind: 'joined', rootFingerprint: st.root_fingerprint ?? s.check?.root_fingerprint ?? '' });
      }
    } catch (e) { setErr(asError(e)); }
    setBusy(false);
  };

  const gate = s.step === 'access' && accessRequired(s) ? `Next unlocks once a check gets in to ${hostOf(accessURL(s))}` : '';
  return (
    <FacetColumns testId="fleet-wizard"
      left={<><IndexHeading>New fleet</IndexHeading>
        <StepRail steps={steps.map(id => ({ id, label: FLEET_STEP_LABEL[id], summary: stepSummary(s, id) }))}
          active={active} onGoto={id => goto(id as FleetStepId)} /></>}>
      <div style={wizardBody}>
        {s.step === 'start' && <StepStart s={s} fleet={fleet} onChoose={c => dispatch({ type: 'CHOOSE', choice: c })} />}
        {s.step === 'hosting' && (
          <StepHosting s={s} fleet={fleet} checking={checking} err={checkErr}
            onHosting={h => dispatch({ type: 'SET_HOSTING', hosting: h })}
            onUrl={url => { setCheckErr(null); dispatch({ type: 'SET_FORGE_URL', url }); }}
            onConnect={() => { void probeForge(); }} />
        )}
        {s.step === 'bundle' && (
          <StepBundle s={s} checking={checking} err={checkErr}
            onLoaded={(bundle, source) => { setCheckErr(null); dispatch({ type: 'BUNDLE_LOADED', bundle, source }); }}
            onLoadErr={e => setCheckErr(e)} onCheck={() => { void checkBundle(); }} />
        )}
        {s.step === 'access' && (
          <StepAccess s={s} checking={checking} err={checkErr} dispatch={dispatch} onCheck={() => { void checkAccess(); }} />
        )}
        {s.step === 'review' && <StepReview s={s} fleet={fleet} err={err} />}

        <div style={wizardFooter}>
          {active > 0 && <button type="button" style={btn(busy)} disabled={busy} onClick={() => { setCheckErr(null); dispatch({ type: 'BACK' }); }}>Back</button>}
          {s.step === 'review' ? (
            <button type="button" data-testid="fleet-wizard-primary" style={btn(busy, 'primary')} disabled={busy} onClick={() => { void primary(); }}>
              {busy ? (s.choice === 'create' ? 'Creating…' : 'Joining…') : err ? 'Try again' : s.choice === 'create' ? 'Create fleet' : 'Join fleet'}
            </button>
          ) : next && (
            <button type="button" data-testid="fleet-wizard-next" style={btn(!!gate, 'primary')} disabled={!!gate} title={gate || undefined}
              onClick={() => { setCheckErr(null); dispatch({ type: 'NEXT' }); }}>Next</button>
          )}
          <button type="button" style={btn(busy)} disabled={busy} onClick={onCancel}>Cancel</button>
        </div>
      </div>
    </FacetColumns>
  );
}

type S = FleetWizardState;

function StepStart({ s, fleet, onChoose }: { s: S; fleet: FleetStatus | null; onChoose: (c: 'create' | 'join') => void }) {
  const noListener = !fleet?.host.listener_on;
  const noAddress = (fleet?.host.external_addresses ?? []).length === 0;
  return (
    <div data-testid="fleet-step-start">
      <SegmentedChoice label="Fleet" value={s.choice} onChange={onChoose} options={[
        { value: 'create', tone: 'neutral', testid: 'fleet-choice-create', title: 'Create a fleet', sub: 'This instance becomes its master' },
        { value: 'join', tone: 'remote', testid: 'fleet-choice-join', title: 'Join a fleet', sub: "With a bundle from the fleet's master" },
      ]} />
      <div style={segDisclosure}>
        {s.choice === 'create' ? (
          <>
            <p style={fieldHint}>This instance becomes the fleet's master and its first member: it holds the fleet's root key, enrolls every other member and merges their branches. The fleet repository is called fleet.</p>
            {fleet && (noListener || noAddress) && (
              <div style={{ marginTop: 12 }}>
                <OutcomeCard tone="ask" testid="fleet-start-cannot-host" headline="This instance can create a fleet, but cannot host one yet"
                  body={`Hosting needs the mTLS listener${noAddress ? ' and an external address' : ''}. Choose a forge on the next step, or set the Fleet port in Settings → Network first.`} />
              </div>
            )}
          </>
        ) : (
          <>
            <div style={{ ...cardLabel, marginTop: 14 }}>Send these to the fleet's master</div>
            <Kv testid="fleet-start-identity" rows={[
              ['Agent id', fleet ? <CopyValue key="a" text={fleet.agent_id} label="Copy agent id" testid="fleet-start-agent" /> : null],
              ['Public key', fleet?.public_key ? <CopyValue key="k" text={fleet.public_key} label="Copy public key" testid="fleet-start-key" /> : null],
            ]} />
            {fleet?.public_key && <CopyBoth text={`${fleet.agent_id}\n${fleet.public_key}`} />}
            <p style={fieldHint}>The master enrolls them (Fleet → Members → Enroll a member) and sends back one bundle, plus the fleet root's fingerprint by another channel. Continue when you have the bundle.</p>
          </>
        )}
      </div>
    </div>
  );
}

function CopyBoth({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div style={{ ...btnRow, marginTop: 8 }}>
      <button type="button" data-testid="fleet-copy-both" style={btn(false)} onClick={() => {
        navigator.clipboard.writeText(text)
          .then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); })
          .catch(() => { /* clipboard unavailable */ });
      }}>{copied ? 'Copied both' : 'Copy both'}</button>
    </div>
  );
}

function StepHosting({ s, fleet, checking, err, onHosting, onUrl, onConnect }: {
  s: S; fleet: FleetStatus | null; checking: boolean; err: Error | null;
  onHosting: (h: 'here' | 'forge') => void; onUrl: (u: string) => void; onConnect: () => void;
}) {
  const serve = fleet?.host.would_serve ?? [];
  const listenerOn = !!fleet?.host.listener_on;
  const hasAddress = (fleet?.host.external_addresses ?? []).length > 0;
  return (
    <div data-testid="fleet-step-hosting">
      <SegmentedChoice label="Hosting" value={s.hosting} onChange={onHosting} options={[
        { value: 'here', tone: 'neutral', testid: 'fleet-hosting-here', title: 'This instance hosts it', sub: 'Members push to this instance' },
        { value: 'forge', tone: 'remote', testid: 'fleet-hosting-forge', title: 'A forge', sub: 'GitHub, GitLab or any git host stores it' },
      ]} />
      <div style={segDisclosure}>
        {s.hosting === 'here' ? (
          <>
            <p style={fieldHint}>Members push to this instance over the fleet listener; it judges and merges their branches into main. Nothing leaves this machine.</p>
            <div style={{ marginTop: 12 }}>
              {listenerOn && serve.length > 0 ? (
                <OutcomeCard tone="good" testid="fleet-hosting-urls" headline={serve.length > 1 ? 'Members will join at one of' : 'Members will join at'}
                  body={serve.length > 1 ? 'Each bundle you issue names one of them; Enroll asks which. A joiner never types it.' : 'The URL goes into every bundle you issue; a joiner never types it.'}>
                  <div style={urlList}>{serve.map(u => <div key={u}>{u}</div>)}</div>
                </OutcomeCard>
              ) : (
                <OutcomeCard tone="ask" testid="fleet-hosting-listener-off"
                  headline={listenerOn ? 'This instance has no external address' : 'The fleet listener is off on this instance'}
                  body={`A fleet hosted here needs the mTLS listener${hasAddress ? '' : ' and an external address'}. Set the Fleet port in Settings → Network; saving applies it. Create is refused until then (${listenerOn ? 'addresses_required' : 'listener_required'}).`} />
              )}
            </div>
          </>
        ) : (
          <>
            <label style={fieldLabel} htmlFor="fleet-forge-url">Remote URL</label>
            <input id="fleet-forge-url" data-testid="fleet-forge-url" style={fieldInput} value={s.forgeUrl} placeholder="https://github.com/acme/fleet"
              autoCapitalize="off" autoCorrect="off" spellCheck={false} onChange={e => onUrl(e.target.value)} />
            <div style={{ ...btnRow, marginTop: 10 }}>
              <button type="button" data-testid="fleet-forge-connect" style={btn(!s.forgeUrl.trim() || checking, 'primary')}
                disabled={!s.forgeUrl.trim() || checking} onClick={onConnect}>{checking ? 'Checking…' : 'Connect'}</button>
            </div>
            <p style={fieldHint}>GitHub, GitLab, Bitbucket, or any bare repo reachable over HTTPS.</p>
            {err && (
              <OutcomeCard tone="bad" testid="fleet-forge-unreachable" headline={hostOf(s.forgeUrl) ? `Couldn't reach ${hostOf(s.forgeUrl)}` : 'Could not reach that remote'}
                detail={err.message} />
            )}
            <div style={fieldCaveat}>
              The remote needs a <b style={{ color: '#8b9199' }}>main</b> branch — one commit is enough, and ticking "add a README" is the quickest way to get one. The fleet's master is the only writer of main: set the forge's branch protection so only this instance's credentials may push it.
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function StepBundle({ s, checking, err, onLoaded, onLoadErr, onCheck }: {
  s: S; checking: boolean; err: Error | null;
  onLoaded: (bundle: string, source: string) => void; onLoadErr: (e: Error) => void; onCheck: () => void;
}) {
  const file = useRef<HTMLInputElement>(null);
  const paste = async () => {
    try {
      const t = await navigator.clipboard.readText();
      if (!t.trim()) { onLoadErr(new Error('The clipboard is empty.')); return; }
      onLoaded(t, 'clipboard');
    } catch (e) { onLoadErr(e instanceof Error ? e : new Error(String(e))); }
  };
  const pick = async (f: File | undefined) => {
    if (!f) return;
    try {
      const t = await readTextFile(f);
      if (!t.trim()) { onLoadErr(new Error(`${f.name} is empty.`)); return; }
      onLoaded(t, f.name);
    } catch (e) { onLoadErr(e instanceof Error ? e : new Error(String(e))); }
    if (file.current) file.current.value = '';
  };
  const code = (err as { code?: string } | null)?.code ?? '';
  const card = err && code ? checkRefusalCard(code, bundleHost(s.bundle)) : null;
  return (
    <div data-testid="fleet-step-bundle">
      <label style={{ ...fieldLabel, marginTop: 0 }}>Bundle</label>
      <div style={btnRow}>
        <button type="button" data-testid="fleet-bundle-paste" style={btn(checking)} disabled={checking} onClick={() => { void paste(); }}>Paste from clipboard</button>
        <label style={{ ...btn(checking), display: 'inline-block' }}>
          Choose file…
          <input ref={file} data-testid="fleet-bundle-file" type="file" style={{ display: 'none' }} onChange={e => { void pick(e.target.files?.[0]); }} />
        </label>
      </div>
      {s.bundle ? (
        <div data-testid="fleet-bundle-loaded" style={fieldHint}>
          {s.bundleSource === 'clipboard' ? 'Loaded from the clipboard.' : `Loaded from ${s.bundleSource}.`} The bundle is sent with the join and never shown.
        </div>
      ) : (
        <div style={fieldHint}>The bundle the fleet's master sent you. It carries the fleet's URL and this instance's certificate; you type nothing else.</div>
      )}
      {s.bundle && (
        <div style={{ ...btnRow, marginTop: 12 }}>
          <button type="button" data-testid="fleet-bundle-check" style={btn(checking, 'primary')} disabled={checking} onClick={onCheck}>{checking ? 'Checking…' : 'Check'}</button>
        </div>
      )}
      {err && (
        <div style={{ marginTop: 12 }}>
          <OutcomeCard tone="bad" testid="fleet-bundle-refused" headline={card ? card.headline : err.message}
            detail={card ? err.message : undefined}>
            {card?.hint && <div style={{ ...fieldHint, marginTop: 0 }}>{card.hint}</div>}
            {code && <div data-testid="fleet-bundle-code" style={codeLine}>{code}</div>}
          </OutcomeCard>
        </div>
      )}
      {s.check && (
        <div style={{ marginTop: 12 }}>
          <OutcomeCard tone="good" testid="fleet-bundle-reached" headline={`Reached ${s.check.host}`} url={s.check.url}
            body={s.check.hosting === 'master'
              ? "A fleet hosted by its master. This instance's certificate is its credential; nothing else is needed."
              : 'The fleet lives on a forge. Your own credentials for it come next.'} />
        </div>
      )}
    </div>
  );
}

function StepAccess({ s, checking, err, dispatch, onCheck }: {
  s: S; checking: boolean; err: Error | null; dispatch: (a: Parameters<typeof fleetWizardReducer>[1]) => void; onCheck: () => void;
}) {
  const url = accessURL(s);
  const host = hostOf(url);
  const asked = s.choice === 'create' ? !!s.probe?.auth_required : !!s.check?.auth_required;
  const ssh = /^[^@/]+@[^:]+:|^ssh:\/\//.test(url);
  return (
    <div data-testid="fleet-step-access">
      {s.accessChecked ? (
        <OutcomeCard tone="good" testid="fleet-access-ok" headline={`Reached ${host} — knomit can push to this remote`} url={url}
          body="main · the master pushes it after every round." />
      ) : asked ? (
        <OutcomeCard tone="ask" testid="fleet-access-ask" headline={`${host} wants credentials`} url={url}
          body={s.choice === 'create'
            ? 'Read access was refused anonymously. Give the token the master uses to push main.'
            : "Reading the fleet repository was refused anonymously. Give this instance's own token for the forge; it pushes its branch there."} />
      ) : (
        <OutcomeCard tone="good" testid="fleet-access-reached" headline={`Reached ${host}`} url={url}
          body={s.choice === 'create' && s.probe?.branches.includes('main') ? 'main · tracked' : undefined} />
      )}
      {err && (
        <div style={{ marginTop: 12 }}>
          <OutcomeCard tone="bad" testid="fleet-access-refused" headline={`${host || 'The remote'} refused those credentials`} url={url} detail={err.message} />
        </div>
      )}
      <label style={fieldLabel} htmlFor="fleet-auth-method">Auth method</label>
      <select id="fleet-auth-method" data-testid="fleet-auth-method" style={fieldInput} value={s.authMethod}
        onChange={e => dispatch({ type: 'SET_AUTH_METHOD', method: e.target.value as FleetAuthMethod })}>
        <option value="">auto-detect</option>
        <option value="none">none</option>
        <option value="token">token</option>
        <option value="basic">basic</option>
        <option value="ssh">ssh</option>
      </select>
      <div style={fieldHint}>
        {asked
          ? "knomit cannot read the fleet repository until it can get in. The token is this instance's own; a bundle is not a forge credential."
          : 'Optional — the check above already succeeded. A private remote over HTTPS still needs a token here to WRITE, even when reading it worked.'}
      </div>
      {s.authMethod === 'basic' && (
        <>
          <label style={fieldLabel} htmlFor="fleet-auth-user">Username</label>
          <input id="fleet-auth-user" data-testid="fleet-auth-user" style={fieldInput} value={s.authUser} placeholder="username"
            onChange={e => dispatch({ type: 'SET_AUTH_USER', user: e.target.value })} />
        </>
      )}
      {s.authMethod !== 'ssh' && s.authMethod !== 'none' && !(ssh && s.authMethod === '') && (
        <>
          <label style={fieldLabel} htmlFor="fleet-auth-token">{s.authMethod === 'basic' ? 'Password' : 'Token'}</label>
          <input id="fleet-auth-token" data-testid="fleet-auth-token" type="password" style={fieldInput} value={s.authToken}
            placeholder="kept in knomit's credential store" onChange={e => dispatch({ type: 'SET_TOKEN', token: e.target.value })} />
        </>
      )}
      <div style={{ ...btnRow, marginTop: 12 }}>
        <button type="button" data-testid="fleet-access-check" style={btn(checking || (asked && !s.authToken && s.authMethod !== 'ssh'))}
          disabled={checking || (asked && !s.authToken && s.authMethod !== 'ssh')} onClick={onCheck}>{checking ? 'Checking…' : 'Check access'}</button>
      </div>
      {asked && !s.accessChecked && <p style={fieldHint}>Next unlocks once a check gets in to {host}.</p>}
    </div>
  );
}

function StepReview({ s, fleet, err }: { s: S; fleet: FleetStatus | null; err: Error | null }) {
  const rootDir = fleet?.root_dir ?? '';
  const serve = fleet?.host.would_serve ?? [];
  const code = (err as { code?: string } | null)?.code ?? '';
  return (
    <div data-testid="fleet-step-review">
      <div style={cardLabel}>What will happen</div>
      {s.choice === 'create' && s.hosting === 'here' && (
        <ol style={will}>
          <li>A fresh fleet root is created in {rootDir} on this machine; this instance is the master.</li>
          <li>The fleet repository is created here, with this instance's record and certificate on main.{serve.length ? ` Members will join at ${serve.join(' or ')}.` : ''}</li>
          <li>Every joiner needs a bundle from Fleet → Members → Enroll a member. Each round, this instance judges and merges their branches into main.</li>
        </ol>
      )}
      {s.choice === 'create' && s.hosting === 'forge' && (
        <ol style={will}>
          <li>A fresh fleet root is created in {rootDir} on this machine; this instance is the master.</li>
          <li>The fleet repository is created and connected to {s.forgeUrl.trim()}. knomit takes its own branch from main and writes this instance's record and certificate there; then it publishes main.</li>
          <li>Members push their own branches to the forge; each round, this instance fetches them, judges, merges into main and pushes main. Only this instance should be allowed to push main.</li>
        </ol>
      )}
      {s.choice === 'join' && s.check && (
        <>
          <ol style={will}>
            <li>The bundle's certificate and root are installed on this instance.</li>
            <li>The fleet repository is cloned from {s.check.url} and follows its main.</li>
            <li>This instance writes its own record on its branch and pushes it once. It counts as a member once the master merges it into main (record pending until then).</li>
            <li>A refused join leaves nothing behind.</li>
          </ol>
          <div data-testid="fleet-review-check" style={card}>
            <div style={cardLabel}>Check before joining</div>
            {/* The bundle's url line is unauthenticated (N6): the operator
                checks the host it names as well as the root. */}
            <Kv rows={[
              ['Fleet root', <span key="r" data-testid="fleet-review-root" style={mono}>{s.check.root_fingerprint}</span>],
              ['Host', <span key="h" data-testid="fleet-review-host" style={mono}>{s.check.host}</span>],
            ]} />
            <div style={fieldHint}>Compare it with the fingerprint the fleet's master gave you by another channel. If it differs, do not join: the bundle did not come from your master.</div>
          </div>
        </>
      )}
      <Refusal err={err} testid="fleet-wizard-error"
        note={s.choice === 'create' ? 'No fleet was created. You can change something and try again.' : joinRefusalNote(code)} />
      {(code === 'listener_required' || code === 'addresses_required') && (
        <div style={fieldHint}>Set the Fleet port and an external address in Settings → Network; saving applies them. Or choose a forge on the Hosting step.</div>
      )}
    </div>
  );
}

const btnRow: CSSProperties = { display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' };
const urlList: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#6f9a7d', overflowWrap: 'anywhere' };
const will: CSSProperties = { margin: '8px 0 0', paddingLeft: 20, fontSize: 13, color: '#ccc', lineHeight: 1.7 };
const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, overflowWrap: 'anywhere' };
const codeLine: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#a08750', marginTop: 6 };
