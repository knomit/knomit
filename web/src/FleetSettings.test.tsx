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
  host: { listener_on: true, external_addresses: ['https://kb-01.acme.example:7443'] },
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
    expect(document.body.textContent).not.toContain('knomit+');
  });

  // SABOTAGE: drop the !local guards -> the buttons render -> red.
  it('over TCP (local false): one ask-tone block first and no buttons at all', () => {
    stubFleetFetch({});
    mount({ ...hub, local: false });
    const note = screen.getByTestId('fleet-settings-browser');
    expect(note).toHaveAttribute('data-tone', 'ask');
    expect(note).toHaveTextContent('Fleet changes need the desktop app on this machine');
    expect(note).toHaveTextContent('enroll, revoke, the master role and decommission are served only to the desktop app');
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
    expect(screen.getByTestId('fleet-hosted-by')).toHaveTextContent("kb-01.acme.example — the fleet's master");
    expect(screen.getByTestId('fleet-import')).toBeInTheDocument();
    expect(screen.getByTestId('fleet-leave')).toBeInTheDocument();
    expect(screen.queryByTestId('fleet-export')).toBeNull();
    expect(screen.queryByTestId('fleet-decommission')).toBeNull();
  });

  // host.would_serve names where members will push; the page builds no URL.
  // SABOTAGE: build the URL from external_addresses -> the would_serve text is
  // missing -> red.
  it('Host the fleet here names the server\'s would_serve URLs and calls DELETE /repos/fleet/origin', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'DELETE /api/v1/repos/fleet/origin': { status: 204 },
    });
    const onChanged = mount({ ...member, master: true, host: { listener_on: true, external_addresses: ['https://ci.example:7443'], would_serve: ['https://served.example:7443/git/fleet'] } });
    fireEvent.click(await screen.findByTestId('fleet-host-here'));
    const confirm = screen.getByTestId('fleet-host-confirm');
    expect(confirm).toHaveTextContent('members push to https://served.example:7443/git/fleet from now on');
    expect(screen.queryByTestId('fleet-host-listener-off')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-host-submit'));
    await waitFor(() => expect(calls.map(c => c.route)).toContain('DELETE /api/v1/repos/fleet/origin'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('Host here asks for the listener before the click when it is off, and shows a refusal with its code', async () => {
    stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': ORIGIN_CERT,
      'DELETE /api/v1/repos/fleet/origin': problem(422, 'listener_required', 'a fleet hosted here needs the mTLS listener'),
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
    const created = vi.fn(() => 'blob:x');
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn() }));
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-export'));
    expect(screen.getByTestId('fleet-export-confirm')).toHaveTextContent('A fleet has one master. Copy the key to the new master and remove it from the old one; knomit does not check that you did.');
    fireEvent.click(screen.getByTestId('fleet-export-save'));
    const done = await screen.findByTestId('fleet-export-done');
    expect(calls.map(c => c.route)).toContain('GET /api/v1/fleet/root');
    expect(done).toHaveTextContent('fleet-root-a3f9c0d12e7b.json · SHA256:root');
    expect(screen.getByTestId('fleet-export-files')).toHaveTextContent('root.key · root.crt · crl.pem · issued.jsonl · revoked.jsonl · extra.txt');
    expect(created).toHaveBeenCalled();
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
  });
});
