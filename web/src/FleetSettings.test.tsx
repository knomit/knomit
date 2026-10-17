import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetSettings } from './FleetSettings';
import type { FleetStatus } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';
import { HOST_HERE_COPY } from './fleetText';

// The fleet's Settings tab. Every card and control is decided by GET /fleet
// as the server says it: `local` (any control at all), `master`, `hosting`,
// `follows_host` (the stale-host card) and `host.would_serve` (the Host-here
// confirm). The page builds no URL and compares nothing.

const hub: FleetStatus = {
  state: 'registered', agent_id: 'a3f9c0d12e7b', fleet_repo: 'fleet', master: true, local: true, hosting: 'none',
  record_state: 'active', public_key: 'ssh-ed25519 AAAAmaster', root_dir: '/home/k/fleet', root_fingerprint: 'SHA256:root',
  host: { listener_on: true, listening: true, external_addresses: ['https://kb-01.acme.example:7443'] },
  fleet_urls: ['https://kb-01.acme.example:7443/git/fleet'], follows_host: true,
};
const member: FleetStatus = {
  ...hub, agent_id: '5be2d8f4a901', master: false, hosting: 'https://kb-01.acme.example:7443/git/fleet',
  host: { listener_on: false, external_addresses: [] },
};

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function mount(fleet: FleetStatus) {
  const onChanged = vi.fn();
  render(<FleetSettings fleet={fleet} onChanged={onChanged} />);
  return onChanged;
}

const ORIGIN_CERT = { body: { name: 'origin', url: 'https://kb-01.acme.example:7443/git/fleet', branch: 'main', auth_method: 'cert' } };

describe('FleetSettings', () => {
  it('the master hub: its standing, its URLs, the root, and Export, Stand down, Decommission', () => {
    stubFleetFetch({});
    mount(hub);
    expect(screen.getByTestId('fleet-settings-role')).toHaveTextContent('master');
    expect(screen.getByTestId('fleet-agent-id')).toHaveTextContent('a3f9c0d12e7b');
    expect(screen.getByTestId('fleet-hosted-by')).toHaveTextContent('this instance — members push to it');
    expect(screen.getByTestId('fleet-settings-hosting')).toHaveTextContent('https://kb-01.acme.example:7443/git/fleet');
    expect(screen.getByTestId('fleet-settings-hosting')).toHaveTextContent('nowhere — it is the host');
    expect(screen.getByTestId('fleet-settings-hosting')).toHaveTextContent('/home/k/fleet');
    expect(screen.getByTestId('fleet-export')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-standdown')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-decommission')).toBeInTheDocument();
    expect(screen.queryByTestId('fleet-leave')).toBeNull();
    expect(screen.queryByTestId('fleet-host-here')).toBeNull();
    expect(screen.queryByTestId('fleet-follows-old-host')).toBeNull();
    expect(screen.queryByTestId('fleet-hub-listener-down')).toBeNull();
    expect(document.body.textContent).not.toContain('knomit+');
  });

  // Review B1: "members push to it" only while the fleet listener is UP
  // (host.listening), not merely configured (listener_on).
  // SABOTAGE: read listener_on for listening -> "members push to it" with the
  // listener down -> red.
  it('the hub with its fleet listener down says members cannot push yet, and how to start it', () => {
    stubFleetFetch({});
    mount({ ...hub, host: { ...hub.host, listening: false, listener_reason: 'no_certificate' } });
    expect(screen.getByTestId('fleet-hosted-by')).toHaveTextContent('this instance — its fleet listener is not running, so members cannot push to it yet');
    const card = screen.getByTestId('fleet-hub-listener-down');
    expect(card).toHaveAttribute('data-tone', 'ask');
    expect(card).toHaveTextContent('The fleet listener is not running');
    expect(card).toHaveTextContent('restart knomit');
  });

  // Review B2: the old hub reaches Point at the new host (it has no origin by
  // definition), with the after-a-switch hint; the hub refusal of Stand down
  // offers it inline. Point at is the fleet's own flow: probe-origin (a read),
  // then POST /fleet/follow {url}.
  // SABOTAGE: offer Point at only when !hub -> the hub has no button -> red;
  // send PUT /repos/fleet/origin -> no /fleet/follow call -> red.
  // Stand down comes first (user ruling 2026-10-16): the hub that stood down
  // is no longer the master, and Point at is still its next step.
  // SABOTAGE: offer Point at on the hub only while it is the master -> the
  // stood-down hub has none -> red.
  it('the hub offers Point at the new host, and still does once it stood down', async () => {
    const { calls } = stubFleetFetch({
      'POST /api/v1/repos:probe-origin': { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], fleet_peer: true } },
      'POST /api/v1/fleet/follow': problem(422, 'not_this_fleet', 'https://other.example/git/fleet is not an instance of this fleet'),
    });
    const { unmount } = render(<FleetSettings fleet={hub} onChanged={vi.fn()} />);
    expect(screen.getByTestId('fleet-point-at-hint')).toHaveTextContent('After this instance stood down and another instance hosts the fleet');
    unmount();
    const onChanged = mount({ ...hub, master: false });
    fireEvent.click(screen.getByTestId('fleet-point-at'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveTextContent('Fleet URL (from the new master\'s Settings → Hosting)');
    expect(screen.getByTestId('fleet-follow-flow')).not.toHaveTextContent('Auth method');
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: 'https://other.example/git/fleet' } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-reached')).toHaveTextContent('Reached other.example');
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    expect(await screen.findByTestId('fleet-follow-error-code')).toHaveTextContent('not_this_fleet');
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: 'https://other.example/git/fleet' });
    expect(onChanged).not.toHaveBeenCalled();
  });

  // Host here on a copy that follows another instance: with no stand-down on
  // this copy's main the confirm says what is left out, and the click sends
  // confirm_loss; with it, nothing is asked.
  // SABOTAGE: drop the oldHost prop -> no loss card, body {} -> red.
  it('Host here while main names another acting master asks the loss confirm', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'POST /api/v1/fleet/host': { body: { ...hub } },
    });
    const newMaster: FleetStatus = { ...member, master: true, host: { listener_on: true, listening: true, external_addresses: ['https://ci.example:7443'], would_serve: ['https://served.example:7443/git/fleet'] } };
    const first = render(<FleetSettings fleet={newMaster} onChanged={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    expect(screen.queryByTestId('fleet-host-loss')).toBeNull();
    expect(screen.getByTestId('fleet-host-submit')).toHaveTextContent('Host the fleet here');
    first.unmount();
    const onChanged = mount({ ...newMaster, acting_master: 'a3f9c0d12e7b' });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    expect(screen.getByTestId('fleet-host-loss')).toHaveTextContent('Main still names a3f9c0d12e7b as the acting master');
    expect(screen.getByTestId('fleet-host-loss')).toHaveTextContent('Hosting now leaves out every change made on kb-01.acme.example:7443 after this copy\'s last sync');
    fireEvent.click(screen.getByTestId('fleet-host-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/host')?.body).toEqual({ confirm_loss: true });
  });

  // A refused push at a host whose master stood down is a plain wait, not a
  // failure to retry.
  // SABOTAGE: ignore last_error_stood_down -> "Retrying on the next sync." -> red.
  it('a push refused because the master stood down says to point at the new host', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, last_error: "Push: knomit: this copy of the fleet repository \"fleet\": its master stood down, and it takes no push", last_error_stood_down: true });
    expect(screen.getByTestId('fleet-last-error-stood-down')).toHaveTextContent('Once the new master hosts the fleet, point this copy at it');
    expect(screen.queryByTestId('fleet-last-error')).toBeNull();
  });

  // GET /fleet acting_master: the master marker on main, or "no master".
  // SABOTAGE: drop the Acting master row -> red.
  it('Settings shows the acting master, or no master', () => {
    const { unmount } = render(<FleetSettings fleet={{ ...hub, acting_master: hub.agent_id }} onChanged={vi.fn()} />);
    expect(screen.getByTestId('fleet-acting-master')).toHaveTextContent('a3f9c0d12e7b (this instance)');
    unmount();
    mount({ ...hub, master: false });
    expect(screen.getByTestId('fleet-acting-master')).toHaveTextContent('no master');
    expect(screen.getByTestId('fleet-hosted-by')).toHaveTextContent('its master stood down, so it takes no pushes');
  });

  // #531 re-review N2: on a root holder the stood-down notice names its own
  // way to the role, never "point elsewhere".
  // SABOTAGE: drop the holder branch of the notice -> red.
  it('the stood-down notice on a root holder says host here, not point at', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, root_held: true, master_paused: 'not_hosting', last_error: 'x: its master stood down, and it takes no push', last_error_stood_down: true });
    expect(screen.getByTestId('fleet-last-error-stood-down')).toHaveTextContent('host the fleet here (Hosting, below) to become the master');
    expect(screen.getByTestId('fleet-last-error-stood-down')).not.toHaveTextContent('point this copy at it');
  });

  // #531 re-review N4: the loss card says how long the wait for a sync is.
  // SABOTAGE: drop the interval -> red.
  it('the loss card names how long the next sync can take', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, master: false, root_held: true, master_paused: 'not_hosting', acting_master: 'a3f9c0d12e7b', sync_interval_seconds: 300,
      host: { listener_on: true, listening: true, external_addresses: ['https://ci.example:7443'], would_serve: ['https://served.example:7443/git/fleet'] } });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    expect(screen.getByTestId('fleet-host-loss')).toHaveTextContent("wait for this copy's next sync (within 5 min)");
  });

  // User ruling 6 on a forge: a root holder main does not name is a member;
  // the Export steps put the stand-down BEFORE the import (#531 re-review S4).
  // SABOTAGE: show the self-hosted order on a forge -> red.
  it('a forge root holder main does not name says so, and the forge export order stands down first', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: 'https://github.com/acme/fleet', branch: 'main', auth_method: 'token' } } });
    const forgeCopy: FleetStatus = { ...member, hosting: 'https://github.com/acme/fleet', root_held: true, acting_master: 'a3f9c0d12e7b' };
    const { unmount } = render(<FleetSettings fleet={forgeCopy} onChanged={vi.fn()} />);
    expect(await screen.findByTestId('fleet-root-not-named')).toHaveTextContent("The fleet's main names a3f9c0d12e7b as the master");
    unmount();
    mount({ ...forgeCopy, master: true, root_held: true, acting_master: forgeCopy.agent_id });
    fireEvent.click(await screen.findByTestId('fleet-export'));
    await waitFor(() => expect(screen.getByTestId('fleet-export-confirm')).toHaveTextContent('Here: Stand down.'));
    const text = screen.getByTestId('fleet-export-confirm').textContent ?? '';
    expect(text.indexOf('Here: Stand down.')).toBeLessThan(text.indexOf('Import root key. It takes the role at its next sync.'));
  });

  // SABOTAGE: drop the master_paused card -> red.
  it('a master whose copy follows another instance says it does no master work', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    const { unmount } = render(<FleetSettings fleet={{ ...member, master: true, master_paused: 'not_hosting', acting_master: 'a3f9c0d12e7b' }} onChanged={vi.fn()} />);
    expect(screen.getByTestId('fleet-master-paused')).toHaveTextContent("names a3f9c0d12e7b as the acting master: it merges, enrolls and revokes nothing");
    unmount();
    mount({ ...member, master: true, master_paused: 'not_hosting' });
    expect(screen.getByTestId('fleet-master-paused')).toHaveTextContent('the fleet has no acting master: host the fleet here to become it');
  });

  it('Point at the new host succeeds through POST /fleet/follow and returns to Settings', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'POST /api/v1/repos:probe-origin': { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], fleet_peer: true } },
      'POST /api/v1/fleet/follow': { body: { ...member, hosting: 'https://new.example/git/fleet' } },
    });
    const onChanged = mount(member);
    fireEvent.click(await screen.findByTestId('fleet-point-at'));
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: 'https://new.example/git/fleet' } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    fireEvent.click(await screen.findByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: 'https://new.example/git/fleet' });
    expect(screen.queryByTestId('fleet-follow-flow')).toBeNull();
  });

  // SABOTAGE: drop the !local guards -> the buttons render -> red.
  it('over TCP (local false): one ask-tone block first and no buttons at all', () => {
    stubFleetFetch({});
    mount({ ...hub, local: false });
    const note = screen.getByTestId('fleet-settings-browser');
    expect(note).toHaveAttribute('data-tone', 'ask');
    expect(note).toHaveTextContent('Fleet changes need the desktop app on this machine');
    // S4: exactly what is desktop-only, now that no generic repo route
    // changes the fleet repository either.
    expect(note).toHaveTextContent('enrolling and revoking members, hosting the fleet here, moving it to a forge, pointing this copy at a new host, updating its forge access, rebuilding its index, exporting the root key, standing down and decommissioning are served only to the desktop app');
    expect(screen.getByTestId('toc-browser')).toBe(screen.getAllByRole('button').find(b => b.getAttribute('data-testid')?.startsWith('toc-')));
    const actions = screen.queryAllByRole('button').filter(b => !(b.getAttribute('data-testid') ?? '').startsWith('toc-') && b.getAttribute('aria-label') !== 'Copy agent id' && !(b.getAttribute('aria-label') ?? '').startsWith('Copy'));
    expect(actions).toEqual([]);
  });

  // follows_host drives the stale-host card, and the page compares nothing.
  // SABOTAGE: draw the card from fleet_urls vs hosting -> the `undefined` row
  // (no host record to compare) shows it -> red; ignore follows_host -> red.
  it('"This copy follows the old host" appears only when follows_host is false', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    for (const [follows, shown] of [[false, true], [true, false], [undefined, false]] as const) {
      const moved: FleetStatus = { ...member, follows_host: follows, fleet_urls: ['https://ci-runner-7.acme.example:7443/git/fleet'], host_record: ['https://ci-runner-7.acme.example:7443/git/fleet'] };
      const { unmount } = render(<FleetSettings fleet={moved} onChanged={() => {}} />);
      if (shown) {
        const card = screen.getByTestId('fleet-follows-old-host');
        expect(card).toHaveTextContent('This copy follows the old host');
        expect(card).toHaveTextContent('https://kb-01.acme.example:7443/git/fleet');
        expect(card).toHaveTextContent('The fleet is now hosted at https://ci-runner-7.acme.example:7443/git/fleet. Point this copy at the new host, below.');
      } else {
        expect(screen.queryByTestId('fleet-follows-old-host')).toBeNull();
      }
      unmount();
    }
  });

  it('a member following a peer: Point at the new host and Import, Leave; no master controls', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount(member);
    expect(await screen.findByTestId('fleet-point-at')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-hosted-by')).toHaveTextContent("kb-01.acme.example:7443 — the fleet's master");
    expect(screen.getByTestId('fleet-import')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-leave')).toBeInTheDocument();
    expect(screen.queryByTestId('fleet-export')).toBeNull();
    expect(screen.queryByTestId('fleet-decommission')).toBeNull();
  });

  // host.would_serve names where members will push; the page builds no URL.
  // SABOTAGE: build the URL from external_addresses -> the would_serve text is
  // missing -> red.
  it('Host the fleet here names the server\'s would_serve URLs and calls POST /fleet/host', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'POST /api/v1/fleet/host': { body: { ...hub } },
    });
    const onChanged = mount({ ...member, master: true, host: { listener_on: true, listening: true, external_addresses: ['https://ci.example:7443'], would_serve: ['https://served.example:7443/git/fleet'] } });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    const confirm = screen.getByTestId('fleet-host-confirm');
    expect(confirm).toHaveTextContent('members push to https://served.example:7443/git/fleet from now on');
    expect(screen.queryByTestId('fleet-host-listener-off')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-host-submit'));
    await waitFor(() => expect(calls.map(c => c.route)).toContain('POST /api/v1/fleet/host'));
    expect(calls.map(c => c.route)).not.toContain('DELETE /api/v1/repos/fleet/origin');
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  // B1 on Host here: configured but not listening -> no "from now on".
  // SABOTAGE: drop notUp -> "from now on" with the listener down -> red.
  // A master whose copy has an origin may be the new master or the old one
  // already following the new host: the page names the host, never "the old
  // master" (browser walk of the fix: the old hub after Point at read "the
  // old master" for the new one).
  // SABOTAGE: restore "the old master" -> red.
  it('a master with an origin names the host without guessing which master it is', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, master: true });
    expect(await screen.findByTestId('fleet-hosted-by')).toHaveTextContent("kb-01.acme.example:7443 — the fleet's host");
    expect(document.body.textContent).not.toContain('old master');
  });

  it('Host here with the listener configured but not running says members push once it runs', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, master: true, host: { listener_on: true, listening: false, listener_reason: 'no_certificate', external_addresses: ['https://ci.example:7443'], would_serve: ['https://served.example:7443/git/fleet'] } });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    const confirm = screen.getByTestId('fleet-host-confirm');
    expect(confirm).toHaveTextContent('members push to https://served.example:7443/git/fleet once its fleet listener runs');
    expect(confirm).not.toHaveTextContent('from now on');
    expect(screen.getByTestId('fleet-host-listener-down')).toHaveTextContent('restart knomit');
  });

  // The Fleet port is set; only the external address is missing. Settings →
  // Network cannot set that, so the card must not send the user there.
  // SABOTAGE: fold the address back into the listener-off card -> red.
  it('Host here with the Fleet port set but no external address names knomit.toml, not the listener', async () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, master: true, host: { listener_on: true, listener: '0.0.0.0:19279', listening: false, listener_reason: 'no_certificate', external_addresses: [] } });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    expect(screen.queryByTestId('fleet-host-listener-off')).toBeNull();
    expect(screen.getByTestId('fleet-host-needs-address')).toHaveTextContent(HOST_HERE_COPY.addressBody('0.0.0.0:19279'));
  });

  it('Host here asks for the listener before the click when it is off, and shows a refusal with its code', async () => {
    stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'POST /api/v1/fleet/host': problem(422, 'listener_required', 'a fleet hosted here needs the mTLS listener'),
    });
    mount({ ...member, master: true });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    expect(screen.getByTestId('fleet-host-listener-off')).toHaveTextContent('The fleet listener is off on this instance');
    fireEvent.click(screen.getByTestId('fleet-host-submit'));
    expect(await screen.findByTestId('fleet-host-error-code')).toHaveTextContent('listener_required');
    expect(screen.getByTestId('fleet-host-error')).toHaveTextContent('Nothing changed.');
  });

  // The export's file list is the server's, never a fixed one.
  // SABOTAGE: hardcode the five names -> the response's six are not all shown -> red.
  it('Export saves the response and lists the files it carries', async () => {
    const files = { 'root.key': 'K', 'root.crt': 'C', 'crl.pem': 'L', 'issued.jsonl': 'I', 'revoked.jsonl': 'R', 'extra.txt': 'X' };
    const { calls } = stubFleetFetch({ 'GET /api/v1/fleet/root': { body: { dir: '/home/k/fleet', root_fingerprint: 'SHA256:root', files } } });
    // A browser with a save dialog: the save is known to have happened.
    const write = vi.fn().mockResolvedValue(undefined);
    const picker = vi.fn().mockResolvedValue({ name: 'fleet-root-a3f9c0d12e7b.json', createWritable: async () => ({ write, close: vi.fn().mockResolvedValue(undefined) }) });
    vi.stubGlobal('showSaveFilePicker', picker);
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-export'));
    expect(screen.getByTestId('fleet-export-confirm')).toHaveTextContent('A fleet has one master at a time. The old master stands down first');
    fireEvent.click(screen.getByTestId('fleet-export-save'));
    const done = await screen.findByTestId('fleet-export-done');
    expect(calls.map(c => c.route)).toContain('GET /api/v1/fleet/root');
    expect(done).toHaveTextContent('fleet-root-a3f9c0d12e7b.json · SHA256:root');
    expect(screen.getByTestId('fleet-export-files')).toHaveTextContent('root.key · root.crt · crl.pem · issued.jsonl · revoked.jsonl · extra.txt');
    expect(picker).toHaveBeenCalledWith({ suggestedName: 'fleet-root-a3f9c0d12e7b.json' });
    expect(write).toHaveBeenCalledTimes(1);
  });

  // Review S1: "Root key saved" only after a save that succeeded. A download
  // handed to the browser, a cancelled dialog and a failure each stay on the
  // confirm and say so.
  // SABOTAGE: setSaved whatever the outcome -> "Root key saved" -> red.
  it('Export never says "Root key saved" for a download, a cancel or a failure', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/root': { body: { dir: '/home/k/fleet', root_fingerprint: 'SHA256:root', files: { 'root.key': 'K' } } } });
    const abort = Object.assign(new Error('aborted'), { name: 'AbortError' });
    for (const [picker, line] of [
      [undefined, 'fleet-root-a3f9c0d12e7b.json was handed to the browser as a download; check that it arrived.'],
      [vi.fn().mockRejectedValue(abort), 'Not saved: the save was cancelled.'],
      [vi.fn().mockRejectedValue(new Error('disk full')), 'Not saved: disk full'],
    ] as const) {
      if (picker) vi.stubGlobal('showSaveFilePicker', picker);
      else {
        vi.stubGlobal('showSaveFilePicker', undefined);
        vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: vi.fn(() => 'blob:x'), revokeObjectURL: vi.fn() }));
      }
      const { unmount } = render(<FleetSettings fleet={hub} onChanged={() => {}} />);
      fireEvent.click(screen.getByTestId('fleet-export'));
      fireEvent.click(screen.getByTestId('fleet-export-save'));
      expect(await screen.findByTestId('fleet-export-not-saved')).toHaveTextContent(line);
      expect(screen.queryByTestId('fleet-export-done')).toBeNull();
      expect(document.body.textContent).not.toContain('Root key saved');
      unmount();
    }
  });

  // Stand down needs nothing first: the confirm says what stops, and a
  // refusal says the key is still here.
  // SABOTAGE: drop the hub clause of the steps -> red.
  it('Stand down on the hub says what stops, and a refusal keeps the key', async () => {
    const { calls } = stubFleetFetch({ 'DELETE /api/v1/fleet/root': problem(409, 'stand_down_unpublished', 'the commit that records the stand-down did not reach the fleet\'s main') });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-standdown'));
    expect(screen.getByTestId('fleet-standdown-steps')).toHaveTextContent("members' pushes to it are refused until they point at the new host");
    fireEvent.click(screen.getByTestId('fleet-standdown-submit'));
    expect(await screen.findByTestId('fleet-standdown-error-code')).toHaveTextContent('stand_down_unpublished');
    expect(screen.getByTestId('fleet-standdown-error')).toHaveTextContent('The root key is still here.');
    expect(calls.map(c => c.route)).toContain('DELETE /api/v1/fleet/root');
  });

  // Leave means revoke (user ruling 2026-10-16): the copy says what happens,
  // the master's round revokes once the left record is on main, and a rejoin
  // needs only a new enroll.
  // SABOTAGE: restore "The master revokes the certificate." as the row's
  // note -> red.
  it('Leave posts /fleet/leave; its copy says the master revokes once the left record is on main', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'POST /api/v1/fleet/leave': { body: { state: 'standalone', agent_id: '5be2d8f4a901', pushed: true } },
    });
    const onChanged = mount(member);
    expect(screen.getByTestId('fleet-settings')).toHaveTextContent("Once the left record is on the fleet's main, the fleet's acting master revokes the certificate when it runs its round.");
    expect(screen.getByTestId('fleet-settings')).toHaveTextContent('if the record cannot be pushed, the master\'s operator revokes it by hand');
    fireEvent.click(screen.getByTestId('fleet-leave'));
    expect(screen.getByTestId('fleet-leave-confirm')).toHaveTextContent("Once the left record is on the fleet's main, the fleet's acting master revokes the certificate when it runs its round");
    expect(screen.getByTestId('fleet-leave-confirm')).toHaveTextContent('To join again later, the master enrolls this instance again and sends a new bundle.');
    expect(screen.getByTestId('fleet-leave-confirm')).not.toHaveTextContent('the master revokes the old certificate, enrolls');
    expect(screen.getByTestId('fleet-leave-confirm')).not.toHaveTextContent('new agent id');
    fireEvent.click(screen.getByTestId('fleet-leave-submit'));
    await waitFor(() => expect(calls.map(c => c.route)).toContain('POST /api/v1/fleet/leave'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('Decommission needs `fleet` typed, then posts /fleet/decommission', async () => {
    const { calls } = stubFleetFetch({ 'POST /api/v1/fleet/decommission': { body: { state: 'standalone', agent_id: 'a3f9c0d12e7b' } } });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-decommission'));
    expect(screen.getByTestId('fleet-decommission-submit')).toBeDisabled();
    fireEvent.change(screen.getByTestId('fleet-decommission-typed'), { target: { value: 'fleet' } });
    fireEvent.click(screen.getByTestId('fleet-decommission-submit'));
    await waitFor(() => expect(calls.map(c => c.route)).toContain('POST /api/v1/fleet/decommission'));
  });

  it('Import sends the chosen file\'s files with PUT /fleet/root; root_present offers Remove the root key', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'PUT /api/v1/fleet/root': problem(409, 'root_present', 'a fleet root is already here: stand down or decommission first'),
    });
    mount(member);
    fireEvent.click(screen.getByTestId('fleet-import'));
    // Review 3 N1 (ruling 6): installing the key is not becoming the master.
    expect(screen.getByTestId('fleet-import-confirm')).toHaveTextContent('Holding it does not make this instance the master: the master is the instance the fleet\'s main names.');
    expect(screen.getByTestId('fleet-import-confirm')).not.toHaveTextContent('master once that root is on the fleet');
    const doc = JSON.stringify({ root_fingerprint: 'SHA256:root', files: { 'root.key': 'K', 'root.crt': 'C' } });
    fireEvent.change(screen.getByTestId('fleet-import-file'), { target: { files: [new File([doc], 'fleet-root-a.json', { type: 'application/json' })] } });
    expect(await screen.findByTestId('fleet-import-root')).toHaveTextContent('Root SHA256:root');
    fireEvent.click(screen.getByTestId('fleet-import-submit'));
    expect(await screen.findByTestId('fleet-import-error-code')).toHaveTextContent('root_present');
    expect(calls.find(c => c.route === 'PUT /api/v1/fleet/root')?.body).toEqual({ files: { 'root.key': 'K', 'root.crt': 'C' } });
    fireEvent.click(screen.getByTestId('fleet-remove-root'));
    expect(screen.getByTestId('fleet-standdown-confirm')).toHaveTextContent('Remove the root key here? This instance stays a member.');
  });

  it('a revoked member says so, and a sync failure is shown verbatim', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, record_state: 'revoked', last_error: 'fetch https://kb-01: connection refused' });
    expect(screen.getByTestId('fleet-settings-role')).toHaveTextContent('revoked');
    expect(screen.getByTestId('fleet-revoked-notice')).toHaveTextContent("This instance's certificate is revoked: the fleet refuses its pushes.");
    expect(screen.getByTestId('fleet-last-error')).toHaveTextContent('Last sync failed: fetch https://kb-01: connection refused');
    expect(screen.getByTestId('fleet-last-error')).toHaveTextContent('Retrying on the next sync.');
    expect(screen.queryByTestId('fleet-refused-notice')).toBeNull();
  });

  // Review S2: a sync refused at the TLS handshake (last_error_refused, read
  // from the recorded error) says the fleet refuses this instance, and never
  // promises a retry.
  // SABOTAGE: ignore last_error_refused -> "Retrying on the next sync." -> red.
  it('a sync refused at the handshake says the fleet refuses this instance, not "Retrying"', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, last_error: "push: pki: the peer refused this instance's certificate: remote error: tls: bad certificate", last_error_refused: true });
    expect(screen.getByTestId('fleet-refused-notice')).toHaveTextContent("The fleet refuses this instance's certificate: it may have been revoked");
    expect(screen.getByTestId('fleet-last-error')).not.toHaveTextContent('Retrying on the next sync.');
  });

  // PR8 S2: a hub member refused at the handshake never reads main's CRL, so
  // record_state still says active; the server's `revoked` (classified from
  // what this instance holds) is what the page shows: the revoked chip, the
  // plain sentence that the copy stopped syncing and how to come back, and the
  // refusal's own class under the raw error. Never "Retrying".
  // SABOTAGE: read record_state alone for the chip and the notice -> the role
  // reads "member" and no notice -> red; ignore sync_stopped -> the short
  // sentence -> red; ignore last_error_refusal -> the undecided sentence ->
  // red.
  it('a revoked member that cannot read the CRL says revoked, that sync stopped, and how to rejoin', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({
      ...member, record_state: 'active', revoked: true, sync_stopped: true,
      last_error: "sync: pki: the peer refused this instance's certificate: remote error: tls: bad certificate",
      last_error_refused: true, last_error_refusal: 'revoked',
    });
    expect(screen.getByTestId('fleet-settings-role')).toHaveTextContent('revoked');
    expect(screen.getByTestId('fleet-revoked-notice')).toHaveTextContent("This instance's certificate is revoked: the fleet refuses it, so this copy has stopped syncing. To rejoin, leave the fleet here, ask the fleet's master to enroll this instance again, and join with the new bundle. Syncing resumes only after knomit restarts or this instance rejoins.");
    expect(screen.getByTestId('fleet-refused-notice')).toHaveTextContent("The fleet refuses this instance's certificate although it is valid here, most likely because the fleet's master revoked it (a clock that is far off can also cause this). Retrying will not help.");
    expect(screen.getByTestId('fleet-last-error')).not.toHaveTextContent('Retrying on the next sync.');
  });

  // PR8 S2: THIS instance refusing the host's certificate (another root
  // answers there) is "moved", not revoked: no revoked chip or notice.
  // SABOTAGE: show the refused notice only for last_error_refused -> no
  // notice, "Retrying on the next sync." -> red.
  // #536 review SF1 (user-approved copy): the `other` class says expired only
  // when the server says the certificate expired here; another alert gets
  // the plain sentence.
  // SABOTAGE: ignore certificate_expired -> the expired row reads the plain
  // sentence -> red.
  it('the other class says expired only when the certificate expired here', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    const { unmount } = render(<FleetSettings fleet={{ ...member, last_error: "sync: pki: the peer refused this instance's certificate: remote error: tls: bad certificate", last_error_refused: true, last_error_refusal: 'other', certificate_expired: true }} onChanged={vi.fn()} />);
    expect(screen.getByTestId('fleet-refused-notice')).toHaveTextContent("The fleet refuses this instance's certificate, and it has expired here. Retrying will not help; ask the fleet's master.");
    unmount();
    mount({ ...member, last_error: "sync: pki: the peer refused this instance's certificate: remote error: tls: handshake failure", last_error_refused: true, last_error_refusal: 'other' });
    expect(screen.getByTestId('fleet-refused-notice')).toHaveTextContent("The fleet refuses this instance's certificate. Retrying will not help; ask the fleet's master.");
    expect(screen.getByTestId('fleet-refused-notice')).not.toHaveTextContent('expired');
  });

  it('a host presenting another root says the fleet moved, not revoked', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, last_error: 'sync: pki: certificate does not chain to the fleet root: x509: certificate signed by unknown authority', last_error_refusal: 'moved' });
    expect(screen.getByTestId('fleet-settings-role')).toHaveTextContent('member');
    expect(screen.queryByTestId('fleet-revoked-notice')).toBeNull();
    expect(screen.getByTestId('fleet-refused-notice')).toHaveTextContent('answers with a certificate of another fleet root');
    expect(screen.getByTestId('fleet-last-error')).not.toHaveTextContent('Retrying on the next sync.');
  });

  // #528 N1: a push whose commit the host's judge refuses
  // (last_error_commit_refused) is sent again by every sync and refused
  // again, so the page never promises a retry; the raw text stays, since it
  // names the commit, the rule and the reason.
  // SABOTAGE: ignore last_error_commit_refused -> "Retrying on the next
  // sync." -> red.
  it('a commit the host refuses is not "Retrying"', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    const raw = "Push: knomit: commit 5a6d21e8 refused by verify_signatures=enforce: host-record: only the fleet's master writes where the fleet is hosted";
    mount({ ...member, last_error: raw, last_error_commit_refused: true });
    expect(screen.getByTestId('fleet-last-error')).toHaveTextContent('Last sync failed: ' + raw);
    expect(screen.getByTestId('fleet-commit-refused-notice')).toHaveTextContent("The fleet's host refuses a commit on this instance's branch");
    expect(screen.getByTestId('fleet-commit-refused-notice')).toHaveTextContent('When the rule is host-record or an artifact rule, the refusal is permanent');
    // #532: the master revokes by itself only when the left record reaches
    // main; a refused Leave push never gets it there, so the copy says when
    // the master's operator still revokes by hand.
    // SABOTAGE: restore the pre-#532 "(until Leave revokes it by itself)"
    // sentence -> red.
    const notice = screen.getByTestId('fleet-commit-refused-notice');
    expect(notice).toHaveTextContent("leave the fleet here, have the fleet's master enroll this instance again, then join again");
    expect(notice).toHaveTextContent("The master revokes this instance's certificate by itself only once the left record that Leave pushes reaches the fleet's main");
    expect(notice).toHaveTextContent("while this commit is on the branch, that push is refused like every other, so ask the fleet's master to revoke the certificate by hand before the enroll");
    expect(notice).not.toHaveTextContent('until Leave revokes it by itself');
    expect(screen.getByTestId('fleet-last-error')).not.toHaveTextContent('Retrying on the next sync.');
    expect(screen.queryByTestId('fleet-refused-notice')).toBeNull();
  });

  // The judge of the master's OWN commits held main back (#533): the notice
  // names the commit, says main stays where it is and that retrying does not
  // help, and its button is the way out (POST /fleet/drop-commit with that
  // commit). Neither "Retrying" nor the member's leave/revoke advice shows.
  // SABOTAGE: drop the last_error_main_held arm from FleetSettings -> the
  // generic "Retrying on the next sync." shows and there is no button -> red;
  // send another commit than last_error_main_held_commit -> the call body
  // assertion fails -> red.
  it("the master's own refused commit: main is held, and Drop is the way out", async () => {
    const commit = '5a6d21e8c0ffee00112233445566778899aabbcc';
    const raw = `knomit: main was not moved: commit ${commit} refused by the ontology's validations: validation: kb/members/a3f9c0d12e7b/note.md fails the ontology's validation "members-are-policy" at members: a member record is a pragmatic policy fact`;
    const fetch = stubFleetFetch({
      'POST /api/v1/fleet/drop-commit': { body: { dropped: commit, replayed: 2, merges_dropped: 0, published: true } },
    });
    const onChanged = vi.fn();
    const held: FleetStatus = { ...hub, last_error: raw, last_error_main_held: true, last_error_main_held_commit: commit };
    const { rerender } = render(<FleetSettings fleet={held} onChanged={onChanged} />);
    expect(screen.getByTestId('fleet-last-error')).toHaveTextContent(raw);
    const notice = screen.getByTestId('fleet-main-held-notice');
    expect(notice).toHaveTextContent("The fleet's main is held where it is: the fleet's rules refuse commit 5a6d21e8 on this instance's own branch");
    expect(notice).toHaveTextContent('Main does not move while that commit is on the branch');
    expect(notice).toHaveTextContent('retrying does not help: knomit never removes a commit by itself');
    // #533 review S3: what is lost is the commit's own changes; a later
    // commit's edit of the same file is kept.
    expect(notice).toHaveTextContent("Drop it: this instance's branch is rebuilt on main without that commit. That commit's own changes are lost, except where a later commit on this branch edited the same file: the later version is kept. Every other commit not yet on main is kept, members' merges included.");
    expect(screen.getByTestId('fleet-last-error')).not.toHaveTextContent('Retrying on the next sync.');
    expect(screen.queryByTestId('fleet-commit-refused-notice')).toBeNull();

    fireEvent.click(screen.getByTestId('fleet-main-held-drop'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(fetch.calls.find(c => c.route === 'POST /api/v1/fleet/drop-commit')?.body).toEqual({ commit });
    // #533 review N1: onChanged reloads GET /fleet, main moved, last_error is
    // gone and the notice unmounts. The outcome is still shown.
    // SABOTAGE: keep the outcome inside MainHeldNotice -> it unmounts with
    // the notice -> red.
    rerender(<FleetSettings fleet={hub} onChanged={onChanged} />);
    expect(screen.queryByTestId('fleet-main-held-notice')).toBeNull();
    expect(screen.getByTestId('fleet-main-held-done')).toHaveTextContent('✓ Dropped 5a6d21e8. Main moved on.');
  });

  // #533 review 3 SF1: a drop refused with foreign_commit offers the
  // confirmed fallback, leaving the whole merge out, and lists what it loses
  // (author and subject) BEFORE anything runs; only the confirm button sends
  // leave_out_merge, with exactly the listed hashes.
  // SABOTAGE: send the fallback without left_out (or on the first click) ->
  // the call bodies differ -> red; drop the list -> red.
  it('a foreign_commit refusal offers leaving the merge out, listing what is lost, only on confirm', async () => {
    const commit = '5a6d21e8c0ffee00112233445566778899aabbcc';
    const merge = 'aaaaaaaa00000000000000000000000000000000';
    const other = 'bbbbbbbb00000000000000000000000000000000';
    const leaveOut = [
      { commit: merge, author: 'master', agent: 'master', subject: 'Merge agent/alpha' },
      { commit, author: 'alpha', agent: 'alpha', subject: 'alpha work one' },
      { commit: other, author: 'alpha', agent: 'alpha', subject: 'alpha work two' },
    ];
    const fetch = stubFleetFetch({
      'POST /api/v1/fleet/drop-commit': (body: unknown) => (body as { leave_out_merge?: boolean }).leave_out_merge
        ? { body: { dropped: commit, replayed: 1, merges_dropped: 0, left_out: leaveOut, published: true } }
        : problem(409, 'foreign_commit', "commit came in with a merge of agent alpha's branch", { leave_out: leaveOut }),
    });
    const onChanged = vi.fn();
    const raw = `knomit: main was not moved: commit ${commit} refused by verify_signatures=enforce: not-owner: x`;
    const held: FleetStatus = { ...hub, last_error: raw, last_error_main_held: true, last_error_main_held_commit: commit };
    const { rerender } = render(<FleetSettings fleet={held} onChanged={onChanged} />);
    fireEvent.click(screen.getByTestId('fleet-main-held-drop'));
    const offer = await screen.findByTestId('fleet-main-held-leave-out');
    expect(screen.getByTestId('fleet-main-held-error-code')).toHaveTextContent('foreign_commit');
    expect(offer).toHaveTextContent("You can leave the whole merge out instead. These 3 commits are then lost from this instance's branch and never reach main");
    const rows = screen.getAllByTestId('fleet-main-held-leave-out-commit');
    expect(rows.map(r => r.textContent)).toEqual(['aaaaaaaa master: Merge agent/alpha', '5a6d21e8 alpha: alpha work one', 'bbbbbbbb alpha: alpha work two']);
    expect(fetch.calls.filter(c => c.route === 'POST /api/v1/fleet/drop-commit').map(c => c.body)).toEqual([{ commit }]);
    expect(onChanged).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId('fleet-main-held-leave-out-confirm'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(fetch.calls.filter(c => c.route === 'POST /api/v1/fleet/drop-commit').map(c => c.body)).toEqual([
      { commit },
      { commit, leave_out_merge: true, left_out: [merge, commit, other] },
    ]);
    rerender(<FleetSettings fleet={hub} onChanged={onChanged} />);
    expect(screen.getByTestId('fleet-main-held-done')).toHaveTextContent('✓ Left the merge out (3 commits). Main moved on.');
  });

  // Review S5 and the user's chip ruling: a pending record says when this
  // page will show the master's merge, from the server's interval (never a
  // fixed 300 s), and its chip is informational blue, not amber.
  // SABOTAGE: hardcode "every 5 min" -> the 90 s row reads 5 min -> red;
  // draw the pending chip amber -> red.
  it('a pending record names the next sync from sync_interval_seconds, with a blue chip', () => {
    stubFleetFetch({ 'GET /api/v1/repos/fleet/origin': ORIGIN_CERT });
    mount({ ...member, record_state: 'pending', sync_interval_seconds: 90 });
    expect(screen.getByTestId('fleet-pending-hint')).toHaveTextContent("this page shows it after this instance's next sync (every 90 s)");
    const chip = screen.getByTestId('fleet-settings-role').querySelector('[data-tone]') as HTMLElement;
    expect(chip).toHaveTextContent('record pending');
    expect(chip.style.color).toBe('rgb(138, 182, 214)');
    expect(document.body.innerHTML).not.toContain('rgb(226, 192, 122)');
  });
});
