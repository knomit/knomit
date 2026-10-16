import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetSettings } from './FleetSettings';
import type { FleetStatus } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';

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
    expect(card).toHaveTextContent('Restart knomit');
  });

  // Review B2: the old hub reaches Point at the new host (it has no origin by
  // definition), with the after-a-switch hint; the hub refusal of Stand down
  // offers it inline. Point at is the fleet's own flow: probe-origin (a read),
  // then POST /fleet/follow {url}.
  // SABOTAGE: offer Point at only when !hub -> the hub has no button -> red;
  // send PUT /repos/fleet/origin -> no /fleet/follow call -> red.
  it('the hub master offers Point at the new host, and the hub refusal of Stand down leads to it', async () => {
    const { calls } = stubFleetFetch({
      'DELETE /api/v1/fleet/root': problem(409, 'hub', 'this instance hosts the fleet (its copy of fleet has no origin): host the fleet on the new master and point this copy at it first (Fleet → Settings → Point at the new host)'),
      'POST /api/v1/repos:probe-origin': { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], fleet_peer: true } },
      'POST /api/v1/fleet/follow': problem(422, 'not_this_fleet', 'https://other.example/git/fleet is not an instance of this fleet'),
    });
    const onChanged = mount(hub);
    expect(screen.getByTestId('fleet-point-at')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-point-at-hint')).toHaveTextContent('The step after another instance hosts the fleet');
    fireEvent.click(screen.getByTestId('fleet-standdown'));
    fireEvent.click(screen.getByTestId('fleet-standdown-submit'));
    fireEvent.click(await screen.findByTestId('fleet-standdown-point-at'));
    expect(screen.getByTestId('fleet-follow-flow')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-follow-flow')).toHaveTextContent('Fleet URL (from the new master\'s Settings → Hosting)');
    expect(screen.getByTestId('fleet-follow-flow')).not.toHaveTextContent('Auth method');
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: 'https://other.example/git/fleet' } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-reached')).toHaveTextContent('Reached other.example');
    expect(calls.find(c => c.route === 'POST /api/v1/repos:probe-origin')?.body).toEqual({ url: 'https://other.example/git/fleet' });
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    expect(await screen.findByTestId('fleet-follow-error-code')).toHaveTextContent('not_this_fleet');
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: 'https://other.example/git/fleet' });
    expect(calls.map(c => c.route)).not.toContain('PUT /api/v1/repos/fleet/origin');
    expect(onChanged).not.toHaveBeenCalled();
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
    expect(screen.getByTestId('fleet-host-listener-down')).toHaveTextContent('Restart knomit');
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
    expect(screen.getByTestId('fleet-export-confirm')).toHaveTextContent('A fleet has one master. Copy the key to the new master and remove it from the old one; knomit does not check that you did.');
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

  it('Stand down on the hub is refused `hub`, and says the key is still here', async () => {
    stubFleetFetch({ 'DELETE /api/v1/fleet/root': problem(409, 'hub', 'this instance hosts the fleet (its copy of fleet has no origin)') });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-standdown'));
    expect(screen.getByTestId('fleet-standdown-confirm')).toHaveTextContent('This instance still hosts the fleet.');
    fireEvent.click(screen.getByTestId('fleet-standdown-submit'));
    expect(await screen.findByTestId('fleet-standdown-error-code')).toHaveTextContent('hub');
    expect(screen.getByTestId('fleet-standdown-error')).toHaveTextContent('The root key is still here.');
  });

  it('Leave posts /fleet/leave; its confirm says a rejoin needs the old certificate revoked', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'POST /api/v1/fleet/leave': { body: { state: 'standalone', agent_id: '5be2d8f4a901', pushed: true } },
    });
    const onChanged = mount(member);
    fireEvent.click(screen.getByTestId('fleet-leave'));
    expect(screen.getByTestId('fleet-leave-confirm')).toHaveTextContent('the master revokes the old certificate, enrolls this instance again and sends a new bundle');
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
    expect(screen.getByTestId('fleet-last-error')).not.toHaveTextContent('Retrying on the next sync.');
    expect(screen.queryByTestId('fleet-refused-notice')).toBeNull();
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
