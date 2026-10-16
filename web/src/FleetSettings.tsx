import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import { api, type FleetRoot, type FleetStatus } from './api';
import { SettingsPage, type Section } from './SettingsPage';
import { OutcomeCard } from './OutcomeCard';
import { FleetFollowFlow, type FleetFollowMode } from './FleetFollowFlow';
import type { FleetAuthMethod } from './fleetWizardState';
import { Chip, CopyValue, Kv, Refusal } from './fleetParts';
import { ONE_MASTER, hostPortOf, intervalText, listenerNotRunning, readTextFile } from './fleetText';
import { saveTextFile, saveOutcomeLine, type SaveOutcome } from './saveFile';
import { btn, confirmBox, confirmInput, fieldHint, fieldLabel } from './manageStyles';
import { dangerBox } from './settingsStyles';

type Confirm = 'export' | 'import' | 'standdown' | 'host' | 'leave' | 'decommission' | null;

/**
 * FleetSettings is the fleet target's Settings tab: SettingsPage blocks with
 * an index. This instance (its standing), Hosting (where the fleet
 * repository lives, and moving it), Master role (the root key), Danger zone
 * (leave or decommission). The master's and a member's page are ONE design:
 * GET /fleet decides which actions appear, and its `local` alone decides
 * whether any appear at all (the fleet's mutations are served to the desktop
 * app only). Every claim here is the server's: the URLs, whether this copy
 * follows the host (follows_host), what a host-here would serve
 * (host.would_serve).
 */
export function FleetSettings({ fleet, onChanged, onBusyChange }: {
  fleet: FleetStatus;
  onChanged: () => void;
  /** A connect commit is in flight in the Point-at flow: the shell holds. */
  onBusyChange?: (busy: boolean) => void;
}) {
  const local = fleet.local === true;
  const master = fleet.master === true;
  const hub = fleet.hosting === 'none';
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [flow, setFlow] = useState<FleetFollowMode | null>(null);
  // The URL a Move starts from: '' (the operator names the forge), or the
  // forge an interrupted move goes back to (interrupted_move.move_back_to).
  const [moveUrl, setMoveUrl] = useState('');
  const setConnecting = (on: boolean) => setFlow(on ? 'follow' : null);
  const startMove = (to = '') => { setMoveUrl(to); setFlow('move'); };
  const [rebuilding, setRebuilding] = useState(false);
  const [rebuildMsg, setRebuildMsg] = useState('');
  const [rebuildErr, setRebuildErr] = useState<Error | null>(null);
  // Whether this copy's origin is another knomit instance of this fleet (the
  // server decided `cert` at attach) or a forge, and the stored method. Only
  // read for a copy with an origin.
  // Keyed by the origin it was read for, so a stale answer is never used.
  const originKey = hub || !fleet.fleet_repo ? '' : `${fleet.fleet_repo} ${fleet.hosting}`;
  const [peerRead, setPeerRead] = useState<{ key: string; peer: boolean | null; method: string }>({ key: '', peer: null, method: '' });
  useEffect(() => {
    if (!originKey || !fleet.fleet_repo) return;
    let live = true;
    api.getOrigin(fleet.fleet_repo)
      .then(o => { if (live) setPeerRead({ key: originKey, peer: o ? o.auth_method === 'cert' : null, method: o?.auth_method ?? '' }); })
      .catch(() => { if (live) setPeerRead({ key: originKey, peer: null, method: '' }); });
    return () => { live = false; };
  }, [originKey, fleet.fleet_repo]);
  const peerOrigin = originKey && peerRead.key === originKey ? peerRead.peer : null;
  const forge = !hub && peerOrigin === false;
  const storedMethod = forge ? peerRead.method : '';
  const stale = fleet.follows_host === false;
  // The server's verdict that a move onto a forge stopped before the host
  // record (#529 S4): the repair is the master's, never Point at.
  const interrupted = master ? fleet.interrupted_move : undefined;

  if (flow && fleet.fleet_repo) {
    const back = () => setFlow(null);
    return (
      <div data-testid="fleet-connect-flow" style={connectFlow}>
        <FleetFollowFlow fleetRepo={fleet.fleet_repo} mode={flow} onCancel={back}
          initialUrl={flow === 'access' ? fleet.hosting ?? '' : flow === 'move' ? moveUrl : flow === 'follow' && stale ? (fleet.host_record ?? [])[0] ?? '' : ''}
          initialMethod={flow === 'access' && ['none', 'token', 'basic', 'ssh'].includes(storedMethod) ? storedMethod as FleetAuthMethod : ''}
          fromForge={flow === 'move' && forge ? fleet.hosting : undefined}
          hostRecord={stale ? fleet.host_record : undefined}
          onDone={() => { back(); onChanged(); }} onBusyChange={onBusyChange} />
      </div>
    );
  }
  const rebuild = async () => {
    setRebuilding(true); setRebuildMsg(''); setRebuildErr(null);
    try {
      await api.rebuildFleetIndex();
      setRebuildMsg('✓ Rebuild started — re-indexing in the background.');
    } catch (e) { setRebuildErr(e instanceof Error ? e : new Error(String(e))); }
    setRebuilding(false);
  };

  const done = () => { setConfirm(null); onChanged(); };
  const sections: Section[] = [];

  if (!local) {
    sections.push({
      id: 'browser', title: 'Opened in a browser',
      body: <OutcomeCard tone="ask" testid="fleet-settings-browser" headline="Fleet changes need the desktop app on this machine"
        body={`This page reaches knomit over the network, and ${master
          ? 'enrolling and revoking members, hosting the fleet here, moving it to a forge, pointing this copy at a new host, updating its forge access, rebuilding its index, exporting the root key, standing down and decommissioning'
          : 'pointing this copy at a new host, updating its forge access, rebuilding its index, importing the root key and leaving the fleet'} are served only to the desktop app. Everything here is read-only.`} />,
    });
  }

  // ── This instance ──
  const role = fleet.record_state === 'pending' ? <Chip text="record pending" tone="pending" />
    : fleet.record_state === 'revoked' ? <Chip text="revoked" tone="revoked" />
      : master ? <Chip text="master" tone="master" /> : <Chip text="member" tone="member" />;
  const advertised = fleet.record?.addresses ?? fleet.host.external_addresses;
  sections.push({
    id: 'this-instance', title: 'This instance', action: <span data-testid="fleet-settings-role">{role}</span>,
    problem: !!fleet.last_error || fleet.record_state === 'revoked',
    body: (
      <div style={col}>
        <div style={dim}>
          {master
            ? "This instance is the fleet's master: it holds the root key, issues certificates and merges every member's branch into main."
            : "This instance is a member: its record is on the fleet's main, and the master merges its branch."}
        </div>
        <Kv testid="fleet-settings-instance" rows={[
          ['Agent id', <CopyValue key="a" text={fleet.agent_id} label="Copy agent id" testid="fleet-agent-id" />],
          ['Public key', fleet.public_key ? <CopyValue key="k" text={fleet.public_key} label="Copy public key" /> : <span style={dim}>none</span>],
          ['Record on main', fleet.record_state
            ? <span><Chip text={fleet.record_state} tone={fleet.record_state === 'pending' ? 'pending' : fleet.record_state} />
              {fleet.record_state === 'pending' && (
                <span data-testid="fleet-pending-hint" style={dim}> — waiting for the master's round; this page shows it after this instance's next sync{fleet.sync_interval_seconds ? ` (every ${intervalText(fleet.sync_interval_seconds)})` : ''}</span>
              )}</span>
            : <span style={dim}>none</span>],
          ['Advertised addresses', advertised.length ? <span style={mono}>{advertised.join(' ')}</span> : <span style={dim}>none</span>],
        ]} />
        {fleet.notice && <div data-testid="fleet-notice" style={dim}>{fleet.notice}</div>}
        {fleet.record_state === 'revoked' && (
          <div data-testid="fleet-revoked-notice" style={{ fontSize: 12, color: '#e8a0a0' }}>This instance's certificate is revoked: the fleet refuses its pushes.</div>
        )}
        {fleet.last_error && fleet.last_error_fleet_moved && (
          // The old host refused this copy's push because the fleet moved
          // (#529 S5): the plain sentence, never the raw refusal with a retry.
          <div data-testid="fleet-last-error-moved" role="alert" style={{ ...errBox, marginTop: 0 }}>
            The fleet has moved: the old host no longer takes this copy's pushes. Use Point at the new host (Hosting, below); nothing this copy wrote is lost.
          </div>
        )}
        {fleet.last_error && !fleet.last_error_fleet_moved && (
          <div data-testid="fleet-last-error" role="alert" style={{ ...errBox, marginTop: 0 }}>
            Last sync failed: <span style={mono}>{fleet.last_error}</span>
            {fleet.last_error_refused
              ? <div data-testid="fleet-refused-notice" style={{ color: '#a08a54', fontSize: 12, marginTop: 6 }}>The fleet refuses this instance's certificate: it may have been revoked, or the fleet moved to another root. Retrying will not help; ask the fleet's master.</div>
              : <div style={{ color: '#a08a54', fontSize: 12, marginTop: 6 }}>Retrying on the next sync.</div>}
          </div>
        )}
      </div>
    ),
  });

  // ── Hosting ──
  const urls = fleet.fleet_urls ?? [];
  const listening = fleet.host.listening === true;
  const hostedBy = hub ? (listening ? 'this instance — members push to it' : 'this instance — its fleet listener is not running, so members cannot push to it yet')
    // A stale copy: where main says the fleet is now, which may be a forge
    // or an instance; this page does not know which (#529 N-e).
    : stale ? `${hostPortOf(urls[0] ?? '') || urls[0] || 'elsewhere'} — where the fleet's main says it is hosted now`
      : forge ? "a forge — the master fetches every member's branch, merges, and pushes main"
        // A master with an origin may be the new master still following the
        // old host, or the old master already following the new one: the page
        // cannot tell, so it names the host only (4b9101b3).
        : `${hostPortOf(urls[0] ?? fleet.hosting ?? '') || 'another instance'} — ${master ? "the fleet's host" : "the fleet's master"}`;
  const wouldServe = fleet.host.would_serve ?? [];
  sections.push({
    id: 'hosting', title: 'Hosting', hint: 'where the fleet repository lives', problem: stale,
    body: (
      <div style={col}>
        <Kv testid="fleet-settings-hosting" rows={[
          ['Hosted by', <span key="h" data-testid="fleet-hosted-by">{hostedBy}</span>],
          [urls.length > 1 ? 'Fleet URLs' : 'Fleet URL', urls.length
            ? <span style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>{urls.map(u => <CopyValue key={u} text={u} label="Copy fleet URL" />)}</span>
            : <span style={dim}>none</span>],
          ['This copy syncs from', hub ? <span style={dim}>nowhere — it is the host</span> : <span style={mono}>{fleet.hosting}</span>],
          ['Access', forge
            ? <span key="m" data-testid="fleet-origin-access" style={mono}>{storedMethod || 'auto-detect'}</span>
            : null],
          ['Root fingerprint', fleet.root_fingerprint ? <span style={mono}>{fleet.root_fingerprint}</span> : <span style={dim}>none</span>],
          ['Root directory', master ? <span style={mono}>{fleet.root_dir}</span> : null],
        ]} />
        {hub && !listening && (
          <OutcomeCard tone="ask" testid="fleet-hub-listener-down" headline="The fleet listener is not running"
            body={listenerNotRunning(fleet.host.listener_on, fleet.host.listener_reason)} />
        )}
        {master && hub && (
          <div style={{ ...fieldHint, marginTop: 0 }}>
            {urls.length > 1 ? 'One of these goes into each bundle you issue (Enroll asks which); a joiner never types it.' : 'The URL is in every bundle you issue; a joiner never types it.'}
          </div>
        )}
        {stale && interrupted && (
          // The server's interrupted_move: this master's copy syncs from a
          // forge the host record does not name. The record is what members
          // follow, so the repair goes back to it (#529 S4).
          <OutcomeCard tone="bad" testid="fleet-move-interrupted" headline="The move to the forge did not finish" url={fleet.hosting}
            body={interrupted.record_is_here
              ? `This copy already syncs from ${fleet.hosting}, but the fleet's main still says this instance hosts the fleet, and members follow main. Host the fleet here (below) to put things back as they were; then move the fleet again.`
              : `This copy already syncs from ${fleet.hosting}, but the fleet's main still says the fleet is hosted at ${interrupted.move_back_to ?? (fleet.host_record ?? []).join(', ')}, and members follow main. Move the fleet back there to put things back as they were, or host the fleet here (below).`}>
            {local && interrupted.move_back_to && (
              <div style={{ ...btns, marginTop: 10 }}>
                <button type="button" data-testid="fleet-move-back" style={btn(false, 'primary')} onClick={() => startMove(interrupted.move_back_to)}>
                  Move the fleet back to {hostPortOf(interrupted.move_back_to) || 'that forge'}…
                </button>
              </div>
            )}
          </OutcomeCard>
        )}
        {stale && !interrupted && (
          // follows_host false: the server compared this copy's origin with
          // the host record on main, and they differ.
          <OutcomeCard tone="bad" testid="fleet-follows-old-host" headline="This copy follows the old host" url={fleet.hosting}
            body={`The fleet is now hosted at ${(fleet.host_record ?? urls).join(', ')}. Point this copy at the new host, below${master ? '; then stand down' : ''}. Until then the old host refuses this copy's pushes; nothing it wrote is lost: it pushes its own branch again on its first sync at the new host.`} />
        )}
        {local && master && hub && (
          <div>
            <div style={btns}>
              <button type="button" data-testid="fleet-move-forge" style={btn(false)} onClick={() => startMove()}>Move the fleet to a forge…</button>
            </div>
            <div data-testid="fleet-move-forge-hint" style={fieldHint}>
              This instance stays the master and keeps the root key; the fleet repository moves to a forge repository you name, and members follow it there with their own credentials.
            </div>
          </div>
        )}
        {local && master && forge && !interrupted && (
          <div>
            <div style={btns}>
              <button type="button" data-testid="fleet-move-forge" style={btn(false)} onClick={() => startMove()}>Move the fleet to another forge…</button>
            </div>
            <div data-testid="fleet-move-forge-hint" style={fieldHint}>
              Changing the forge URL is a move: the new forge repository becomes where the fleet is hosted, and every member points at it with its own credential. The review lists what members must do.
            </div>
          </div>
        )}
        {local && forge && !interrupted && (
          <div>
            <div style={btns}>
              <button type="button" data-testid="fleet-update-access" style={btn(false)} onClick={() => setFlow('access')}>Update access…</button>
            </div>
            <div data-testid="fleet-update-access-hint" style={fieldHint}>
              Rotate this instance's token for the forge, or change how it signs in. The forge URL stays as it is{master ? ': to change it, move the fleet to another forge.' : ': if the fleet moved, point this copy at the new host.'}
            </div>
          </div>
        )}
        {local && master && !hub && (
          <div>
            <div style={btns}>
              <button type="button" data-testid="fleet-host-here" style={btn(false, 'primary')} onClick={() => setConfirm('host')}>Host the fleet here…</button>
            </div>
            <div style={fieldHint}>
              The fleet is hosted {forge ? 'on the forge' : `at ${hostPortOf(urls[0] ?? fleet.hosting ?? '') || 'another instance'}`}. Hosting it here removes this copy's origin; members then point at {wouldServe.length ? wouldServe.join(', ') : 'this instance'}.
            </div>
            {confirm === 'host' && <HostConfirm fleet={fleet} onCancel={() => setConfirm(null)} onDone={done} />}
          </div>
        )}
        {/* Point at: the hub master after a switch, any copy following a
            peer, and a MEMBER on a forge (its URL change). Never the master
            on a forge: its URL change is a move (#529 S3, S4). */}
        {local && (hub ? master : peerOrigin !== null && (!forge || !master)) && (
          <div>
            <div style={btns}>
              <button type="button" data-testid="fleet-point-at" style={btn(false)} onClick={() => setConnecting(true)}>Point at the new host…</button>
            </div>
            <div data-testid="fleet-point-at-hint" style={fieldHint}>
              {hub
                ? 'The step after another instance hosts the fleet (it imported the root key and chose Host the fleet here): point this copy at its fleet URL, then stand down. Stand down waits until main says the fleet is hosted where this copy points.'
                : stale
                  ? "The fleet moved: this copy follows it from where main says it is hosted now. On a forge it asks for this instance's own credential."
                  : forge
                    ? "When the master moves the fleet to another forge repository or host: this copy follows it there. On a forge it asks for this instance's own credential."
                    : 'After a master switch: this copy follows the fleet from the URL the new master gives you.'}
            </div>
          </div>
        )}
      </div>
    ),
  });

  // ── Master role ──
  sections.push({
    id: 'master-role', title: 'Master role', hint: 'who holds the root key',
    body: master ? (
      <div style={col}>
        <div style={dim}>The root key is here. To make another member the master, export the key, import it there, then stand down here.</div>
        {local && (
          <div style={btns}>
            <button type="button" data-testid="fleet-export" style={btn(false)} onClick={() => setConfirm('export')}>Export root key…</button>
            <button type="button" data-testid="fleet-standdown" style={btn(false)} onClick={() => setConfirm('standdown')}>Stand down…</button>
          </div>
        )}
        {confirm === 'export' && <ExportConfirm fleet={fleet} forge={forge} onCancel={() => setConfirm(null)} />}
        {confirm === 'standdown' && <StandDownConfirm master hub={hub} onCancel={() => setConfirm(null)} onDone={done}
          onPointAt={local && !forge ? () => { setConfirm(null); setConnecting(true); } : undefined} />}
      </div>
    ) : (
      <div style={col}>
        <div style={dim}>Becoming the master means receiving the root key from the current master.</div>
        {local && (
          <div style={btns}>
            <button type="button" data-testid="fleet-import" style={btn(false)} onClick={() => setConfirm('import')}>Import root key…</button>
          </div>
        )}
        {confirm === 'import' && (
          <ImportConfirm forge={forge} onCancel={() => setConfirm(null)} onDone={done} onRemove={() => setConfirm('standdown')} />
        )}
        {confirm === 'standdown' && <StandDownConfirm master={false} hub={hub} onCancel={() => setConfirm(null)} onDone={done} />}
      </div>
    ),
  });

  // ── Index ── the repository Settings' Index block, for the fleet
  // repository (POST /fleet/index-rebuild: its generic route is refused).
  sections.push({
    id: 'index', title: 'Index', hint: "the search and recall index of this copy of the fleet repository",
    action: local ? (
      <button type="button" data-testid="fleet-rebuild" style={btn(rebuilding)} disabled={rebuilding} onClick={() => { void rebuild(); }}>
        {rebuilding ? 'Rebuilding…' : 'Rebuild'}
      </button>
    ) : undefined,
    body: (
      <div style={col}>
        <div style={{ fontSize: 12.5, color: '#888' }}>
          {rebuildMsg
            ? <span data-testid="fleet-rebuild-status" style={{ color: '#9c9' }}>{rebuildMsg}</span>
            : 'Re-indexing runs in the background; the fleet repository stays readable throughout.'}
        </div>
        <Refusal err={rebuildErr} testid="fleet-rebuild-error" />
      </div>
    ),
  });

  // ── Danger zone ──
  if (local) {
    sections.push({
      id: 'danger', title: 'Danger zone', danger: true,
      body: master ? (
        <div style={dangerBox}>
          <DangerRow title="Decommission the fleet" action={confirm !== 'decommission' && (
            <button type="button" data-testid="fleet-decommission" style={btn(false, 'danger')} onClick={() => setConfirm('decommission')}>Decommission…</button>
          )} note="Deletes the root key and this copy of the fleet repository. Members keep their copies but nobody can enroll, merge or revoke again." />
          {confirm === 'decommission' && <DecommissionConfirm hub={hub} onCancel={() => setConfirm(null)} onDone={done} />}
        </div>
      ) : (
        <div style={dangerBox}>
          <DangerRow title="Leave the fleet" action={confirm !== 'leave' && (
            <button type="button" data-testid="fleet-leave" style={btn(false, 'danger')} onClick={() => setConfirm('leave')}>Leave…</button>
          )} note="Pushes a left record, then removes the fleet repository and this instance's certificate. The master revokes the certificate. Works offline." />
          {confirm === 'leave' && <LeaveConfirm onCancel={() => setConfirm(null)} onDone={done} />}
        </div>
      ),
    });
  }

  return <SettingsPage kind="fleet" sections={sections} testid="fleet-settings" />;
}

function DangerRow({ title, action, note }: { title: string; action: ReactNode; note: string }) {
  return (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <span style={{ fontSize: 13, color: '#ddd' }}>{title}</span>
        <div style={{ flex: 1 }} />
        {action}
      </div>
      <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>{note}</div>
    </>
  );
}

/** useAction runs one fleet mutation for a confirm: busy while it runs, the
 *  refusal kept for the box to show. */
function useAction() {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<Error | null>(null);
  const run = async (f: () => Promise<unknown>, after?: () => void) => {
    setBusy(true); setErr(null);
    try {
      await f();
      setBusy(false);
      after?.();
    } catch (e) {
      setErr(e instanceof Error ? e : new Error(String(e)));
      setBusy(false);
    }
  };
  return { busy, err, run };
}

function Buttons({ busy, label, busyLabel, variant = 'primary', onGo, onCancel, disabled = false, testid }: {
  busy: boolean; label: string; busyLabel: string; variant?: 'primary' | 'danger'; onGo: () => void; onCancel: () => void; disabled?: boolean; testid: string;
}) {
  return (
    <div style={{ ...btns, marginTop: 10 }}>
      <button type="button" data-testid={testid} style={btn(busy || disabled, variant)} disabled={busy || disabled} onClick={onGo}>{busy ? busyLabel : label}</button>
      <button type="button" style={btn(busy)} disabled={busy} onClick={onCancel}>Cancel</button>
    </div>
  );
}

function ExportConfirm({ fleet, forge, onCancel }: { fleet: FleetStatus; forge: boolean; onCancel: () => void }) {
  const { busy, err, run } = useAction();
  const [saved, setSaved] = useState<{ file: string; root: FleetRoot; where: string } | null>(null);
  const [notSaved, setNotSaved] = useState<SaveOutcome | null>(null);
  if (saved) {
    return (
      <div data-testid="fleet-export-done" style={confirmBox}>
        <OutcomeCard tone="good" headline="Root key saved" url={saved.where}
          body={`${saved.file} · ${saved.root.root_fingerprint ?? ''}. On the new master: Fleet → Settings → Master role → Import root key. Then, here: Stand down.`}>
          {/* The files are the server's list, never a fixed one. */}
          <div data-testid="fleet-export-files" style={{ ...mono, color: '#86ab92' }}>{Object.keys(saved.root.files).join(' · ')}</div>
        </OutcomeCard>
        <div style={{ ...btns, marginTop: 10 }}><button type="button" style={btn(false)} onClick={onCancel}>Done</button></div>
      </div>
    );
  }
  const save = () => run(async () => {
    setNotSaved(null);
    const root = await api.exportFleetRoot();
    const file = `fleet-root-${fleet.agent_id}.json`;
    const out = await saveTextFile(file, JSON.stringify({ root_fingerprint: root.root_fingerprint, files: root.files }, null, 2), 'application/json');
    // "Saved" only for a save that succeeded: a download handed to the
    // browser, a cancel or a failure stays on this box and says so.
    if (out.kind === 'saved') setSaved({ file, root, where: out.where });
    else setNotSaved(out);
  });
  return (
    <div data-testid="fleet-export-confirm" style={confirmBox}>
      <div style={lead}>Export the fleet root key for a new master?</div>
      <div style={rule}>{ONE_MASTER}</div>
      <ol style={steps}>
        <li>Save the key file here and carry it to the new master by a channel you trust. It is the fleet's identity: anyone holding it can enroll and revoke.</li>
        <li>On the new master: Fleet → Settings → Master role → Import root key.</li>
        <li>{forge ? 'The new master pushes main to the forge from then on.' : 'The new master then hosts the fleet (its Settings → Hosting), and this copy points at it.'}</li>
        <li>Back here: Stand down removes the key.</li>
      </ol>
      <Refusal err={err} testid="fleet-export-error" />
      {notSaved && <div data-testid="fleet-export-not-saved" role="status" style={{ ...fieldHint, color: notSaved.kind === 'failed' ? '#e2c07a' : '#bbb' }}>{saveOutcomeLine(notSaved)}</div>}
      <Buttons testid="fleet-export-save" busy={busy} label="Save root key…" busyLabel="Saving…" onGo={() => { void save(); }} onCancel={onCancel} />
    </div>
  );
}

function ImportConfirm({ forge, onCancel, onDone, onRemove }: { forge: boolean; onCancel: () => void; onDone: () => void; onRemove: () => void }) {
  const { busy, err, run } = useAction();
  const [file, setFile] = useState<{ name: string; fp: string; files: Record<string, string> } | null>(null);
  const [readErr, setReadErr] = useState('');
  const input = useRef<HTMLInputElement>(null);
  const pick = async (f: File | undefined) => {
    if (!f) return;
    setReadErr(''); setFile(null);
    try {
      const doc = JSON.parse(await readTextFile(f)) as { root_fingerprint?: string; files?: Record<string, string> };
      if (!doc || typeof doc.files !== 'object' || doc.files === null) throw new Error('no files');
      setFile({ name: f.name, fp: doc.root_fingerprint ?? '', files: doc.files });
    } catch {
      setReadErr(`${f.name} is not a root key file from Export root key.`);
    }
    if (input.current) input.current.value = '';
  };
  const code = (err as { code?: string } | null)?.code;
  return (
    <div data-testid="fleet-import-confirm" style={confirmBox}>
      <div style={lead}>Import the fleet root key and make this instance the master?</div>
      <div style={rule}>{ONE_MASTER}</div>
      <label style={{ ...fieldLabel, marginTop: 0 }}>Root key file (from the current master's Export root key)</label>
      <label style={{ ...btn(busy), display: 'inline-block' }}>
        Choose file…
        <input ref={input} data-testid="fleet-import-file" type="file" accept=".json,application/json" style={{ display: 'none' }}
          onChange={e => { void pick(e.target.files?.[0]); }} />
      </label>
      {file && <div data-testid="fleet-import-root" style={fieldHint}>Root <span style={mono}>{file.fp}</span></div>}
      {readErr && <div role="alert" style={{ ...fieldHint, color: '#e2c07a' }}>{readErr}</div>}
      <ol style={steps}>
        <li>The key is installed; this instance is the master once that root is on the fleet's main.</li>
        <li>{forge ? 'This instance pushes main to the forge from its next round.' : 'Then: Settings → Hosting → Host the fleet here, and every member points at this instance.'}</li>
        <li>On the old master: Stand down.</li>
      </ol>
      <Refusal err={err} testid="fleet-import-error" />
      {code === 'root_present' && (
        <div style={{ ...btns, marginTop: 8 }}>
          <button type="button" data-testid="fleet-remove-root" style={btn(false)} onClick={onRemove}>Remove the root key…</button>
        </div>
      )}
      <Buttons testid="fleet-import-submit" busy={busy} label="Import root key" busyLabel="Importing…" disabled={!file}
        onGo={() => { if (file) void run(() => api.importFleetRoot(file.files), onDone); }} onCancel={onCancel} />
    </div>
  );
}

function StandDownConfirm({ master, hub, onCancel, onDone, onPointAt }: { master: boolean; hub: boolean; onCancel: () => void; onDone: () => void; onPointAt?: () => void }) {
  const { busy, err, run } = useAction();
  const code = (err as { code?: string } | null)?.code;
  return (
    <div data-testid="fleet-standdown-confirm" style={confirmBox}>
      <div style={lead}>{master ? 'Stand down as master? The root files are deleted here; this instance stays a member.' : 'Remove the root key here? This instance stays a member.'}</div>
      <div style={rule}>{ONE_MASTER}</div>
      {hub && <div style={{ ...fieldHint, margin: '0 0 8px' }}>This instance still hosts the fleet. Standing down is refused until the new master hosts it and this copy points at the new host (Hosting, above).</div>}
      <Refusal err={err} note={code === 'hub' ? 'The root key is still here.' : undefined} testid="fleet-standdown-error" />
      {code === 'hub' && onPointAt && (
        <div style={{ ...btns, marginTop: 8 }}>
          <button type="button" data-testid="fleet-standdown-point-at" style={btn(false)} onClick={onPointAt}>Point at the new host…</button>
        </div>
      )}
      <Buttons testid="fleet-standdown-submit" busy={busy} label="Stand down" busyLabel="Standing down…" variant="danger"
        onGo={() => { void run(() => api.standDownFleetRoot(), onDone); }} onCancel={onCancel} />
    </div>
  );
}

function HostConfirm({ fleet, onCancel, onDone }: { fleet: FleetStatus; onCancel: () => void; onDone: () => void }) {
  const { busy, err, run } = useAction();
  const serve = fleet.host.would_serve ?? [];
  const listenerOff = !fleet.host.listener_on || fleet.host.external_addresses.length === 0;
  const notUp = !listenerOff && fleet.host.listening !== true;
  return (
    <div data-testid="fleet-host-confirm" style={confirmBox}>
      <div style={lead}>Host the fleet on this instance?</div>
      <ol style={steps}>
        <li>This copy's origin is removed; members push to {serve.length ? serve.join(', ') : 'this instance'}{notUp ? ' once its fleet listener runs' : ' from now on'}.</li>
        <li>Tell every member the new URL: each points its copy at it (Settings → Hosting → Point at the new host).</li>
        <li>The old master points its copy here too, then stands down.</li>
      </ol>
      {listenerOff && (
        <OutcomeCard tone="ask" testid="fleet-host-listener-off" headline="The fleet listener is off on this instance"
          body="Hosting needs the mTLS listener and an external address. Set the Fleet port in Settings → Network; saving applies it." />
      )}
      {notUp && (
        <OutcomeCard tone="ask" testid="fleet-host-listener-down" headline="The fleet listener is not running"
          body={listenerNotRunning(true, fleet.host.listener_reason)} />
      )}
      <Refusal err={err} note="Nothing changed." testid="fleet-host-error" />
      <Buttons testid="fleet-host-submit" busy={busy} label="Host the fleet here" busyLabel="Hosting…"
        onGo={() => { void run(() => api.hostFleetHere(), onDone); }} onCancel={onCancel} />
    </div>
  );
}

function LeaveConfirm({ onCancel, onDone }: { onCancel: () => void; onDone: () => void }) {
  const { busy, err, run } = useAction();
  return (
    <div data-testid="fleet-leave-confirm" style={confirmBox}>
      <div style={lead}>
        Leave the fleet? A left record is pushed once, then the fleet repository and this instance's certificate are removed here, whatever the push did. To join again later, the master revokes the old certificate, enrolls this instance again and sends a new bundle.
      </div>
      <Refusal err={err} testid="fleet-leave-error" />
      <Buttons testid="fleet-leave-submit" busy={busy} label="Leave fleet" busyLabel="Leaving…" variant="danger"
        onGo={() => { void run(() => api.leaveFleet(), onDone); }} onCancel={onCancel} />
    </div>
  );
}

function DecommissionConfirm({ hub, onCancel, onDone }: { hub: boolean; onCancel: () => void; onDone: () => void }) {
  const { busy, err, run } = useAction();
  const [typed, setTyped] = useState('');
  return (
    <div data-testid="fleet-decommission-confirm" style={confirmBox}>
      <div style={lead}>
        Decommission the fleet? The root key is deleted — nobody can enroll, revoke or merge again — and this copy of the fleet repository is removed. Members are not told{hub ? '; their copies stop syncing' : ''}.
      </div>
      <label style={{ ...fieldLabel, marginTop: 0 }} htmlFor="fleet-decommission-typed">Type fleet to confirm</label>
      <input id="fleet-decommission-typed" data-testid="fleet-decommission-typed" style={{ ...confirmInput, fontFamily: 'var(--k-font-mono)' }}
        value={typed} placeholder="fleet" onChange={e => setTyped(e.target.value)} />
      <Refusal err={err} testid="fleet-decommission-error" />
      <Buttons testid="fleet-decommission-submit" busy={busy} label="Decommission fleet" busyLabel="Decommissioning…" variant="danger"
        disabled={typed !== 'fleet'} onGo={() => { void run(() => api.decommissionFleet(), onDone); }} onCancel={onCancel} />
    </div>
  );
}

const col: CSSProperties = { display: 'flex', flexDirection: 'column', gap: 10 };
const dim: CSSProperties = { color: '#888', fontSize: 12 };
const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, overflowWrap: 'anywhere' };
const btns: CSSProperties = { display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' };
const lead: CSSProperties = { fontSize: 13, marginBottom: 8 };
const steps: CSSProperties = { margin: '6px 0 10px 18px', padding: 0, fontSize: 12.5, color: '#ccc', lineHeight: 1.65 };
// The one-master rule is a statement of how the fleet works, not a failure:
// informational, never amber (3f1b3342).
const rule: CSSProperties = {
  fontSize: 12.5, color: '#b8cfe0', background: '#131d26', border: '1px solid #244056', borderRadius: 5,
  padding: '8px 10px', margin: '8px 0 10px', lineHeight: 1.5,
};
const errBox: CSSProperties = {
  padding: '10px 12px', borderRadius: 6, background: '#262013', border: '1px solid #4a3f22',
  color: '#e2c07a', fontSize: 12.5, lineHeight: 1.55,
};
// The Point-at flow fills the facet body and scrolls inside it, as the repo
// page's Connect flow does.
const connectFlow: CSSProperties = {
  flex: '1 1 0', minHeight: 0, height: '100%', overflowY: 'auto', padding: '8px 32px 48px', boxSizing: 'border-box',
};
