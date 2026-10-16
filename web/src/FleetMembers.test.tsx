import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { FleetMembers } from './FleetMembers';
import type { FleetMember, FleetStatus } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';

// The fleet's Members tab, at the fetch seam: what it reads, what it sends,
// and what it offers where. `local` (GET /fleet) alone decides whether the
// master's controls exist.

const master: FleetStatus = {
  state: 'registered', agent_id: 'a3f9c0d12e7b', fleet_repo: 'fleet', master: true, local: true, hosting: 'none',
  record_state: 'active', public_key: 'ssh-ed25519 AAAAmaster', root_dir: '/home/k/fleet',
  host: { listener_on: true, external_addresses: ['https://kb-01.acme.example:7443', 'https://kb-01.lan:7443'] },
  fleet_urls: ['https://kb-01.acme.example:7443/git/fleet', 'https://kb-01.lan:7443/git/fleet'],
};
const rows: FleetMember[] = [
  { agent: 'a3f9c0d12e7b', state: 'active', host: 'kb-01.acme.example', since: '2026-10-07T10:00:00Z', capabilities: { os: 'linux', arch: 'amd64', version: '0.6.0' } },
  { agent: '2c8f6e1a9b47', state: 'enrolled', since: '2026-10-16T08:00:00Z',
    last_refusal: { branch: 'agent/2c8f6e1a9b47', tip: '9e21c0f00aa', agent: '2c8f6e1a9b47', reason: 'not the branch owner', at: '2026-10-16T09:00:00Z' } },
  { agent: '91ab3f5c2d08', state: 'left', host: 'tom-desktop.acme.example' },
  { agent: 'e04d7c93b1f2', state: 'revoked', host: 'build-02.acme.example' },
];
const BUNDLE = '# knomit fleet bundle v1\nurl: https://kb-01.lan:7443/git/fleet\nagent: 5be2d8f4a901\n-----BEGIN CERTIFICATE-----\nSECRETPEMBODY\n-----END CERTIFICATE-----\n';

afterEach(() => { vi.unstubAllGlobals(); });

function mount(fleet: FleetStatus = master) {
  const onChanged = vi.fn();
  render(<FleetMembers fleet={fleet} landing={null} onDismissLanding={() => {}} onChanged={onChanged} />);
  return onChanged;
}

describe('FleetMembers', () => {
  it('reads GET /fleet/members and draws every row with its state, counts and since', async () => {
    const { calls } = stubFleetFetch({ 'GET /api/v1/fleet/members': { body: { members: rows } } });
    mount();
    await screen.findAllByTestId('fleet-member-row');
    expect(calls.map(c => c.route)).toContain('GET /api/v1/fleet/members');
    expect(screen.getByTestId('fleet-member-counts')).toHaveTextContent('1 active · 1 enrolled · 1 left · 1 revoked · 1 refused');
    expect(screen.getAllByTestId('fleet-member-row').map(r => r.getAttribute('data-agent')))
      .toEqual(['a3f9c0d12e7b', '2c8f6e1a9b47', '91ab3f5c2d08', 'e04d7c93b1f2']);
    expect(screen.getByTestId('fleet-member-state-2c8f6e1a9b47')).toHaveTextContent('enrolled');
    expect(screen.getAllByTestId('fleet-member-row')[0]).toHaveTextContent('this instance');
  });

  // #527 F2: the refusal's agent is the refused tip's CLAIMED author.
  it('shows a last refusal with its claimed author', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/members': { body: { members: rows } } });
    mount();
    const r = await screen.findByTestId('fleet-member-refusal-2c8f6e1a9b47');
    expect(r).toHaveTextContent('agent/2c8f6e1a9b47 @ 9e21c0f');
    expect(r).toHaveTextContent('not the branch owner');
    expect(r).toHaveTextContent('claimed by 2c8f6e1a9b47');
    expect(r).toHaveTextContent('left behind until the branch moves');
  });

  it('enrolls with POST /fleet/enroll {agent_id, public_key, address}, and the bundle is copied or saved, never shown', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/fleet/members': { body: { members: rows } },
      'POST /api/v1/fleet/enroll': { status: 201, body: {
        agent_id: '5be2d8f4a901', serial: '0a', bundle: BUNDLE, fleet_url: 'https://kb-01.lan:7443/git/fleet',
        root_fingerprint: 'SHA256:rootfp', published: true,
      } },
    });
    const write = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true });
    const onChanged = mount();
    fireEvent.click(await screen.findByTestId('fleet-enroll-open'));
    fireEvent.change(screen.getByTestId('fleet-enroll-agent'), { target: { value: ' 5be2d8f4a901 ' } });
    fireEvent.change(screen.getByTestId('fleet-enroll-key'), { target: { value: 'ssh-ed25519 AAAAjoiner' } });
    // Two external addresses on the hub: Enroll waits for the choice.
    expect(screen.getByTestId('fleet-enroll-submit')).toBeDisabled();
    fireEvent.change(screen.getByTestId('fleet-enroll-address'), { target: { value: 'https://kb-01.lan:7443' } });
    fireEvent.click(screen.getByTestId('fleet-enroll-submit'));
    const result = await screen.findByTestId('fleet-enroll-result');
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/enroll')?.body)
      .toEqual({ agent_id: '5be2d8f4a901', public_key: 'ssh-ed25519 AAAAjoiner', address: 'https://kb-01.lan:7443' });
    expect(result).toHaveTextContent('Enrolled 5be2d8f4a901');
    expect(screen.getByTestId('fleet-enroll-url')).toHaveTextContent('https://kb-01.lan:7443/git/fleet');
    expect(screen.getByTestId('fleet-enroll-fingerprint')).toHaveTextContent('SHA256:rootfp');
    // The bundle is never displayed.
    expect(document.body.textContent).not.toContain('SECRETPEMBODY');
    expect(document.body.textContent).not.toContain('# knomit fleet bundle');
    expect(document.body.textContent).not.toContain('knomit+');
    fireEvent.click(screen.getByTestId('fleet-copy-bundle'));
    await waitFor(() => expect(write).toHaveBeenCalledWith(BUNDLE));
    expect(onChanged).toHaveBeenCalled();
  });

  it('shows a refused enroll verbatim with its code, and offers the revoke for `enrolled`', async () => {
    stubFleetFetch({
      'GET /api/v1/fleet/members': { body: { members: rows } },
      'POST /api/v1/fleet/enroll': problem(409, 'enrolled', 'agent 2c8f6e1a9b47 is enrolled (serial 6 is not revoked): revoke it first'),
    });
    mount({ ...master, hosting: 'https://forge.example/fleet.git', host: { listener_on: false, external_addresses: [] } });
    fireEvent.click(await screen.findByTestId('fleet-enroll-open'));
    fireEvent.change(screen.getByTestId('fleet-enroll-agent'), { target: { value: '2c8f6e1a9b47' } });
    fireEvent.change(screen.getByTestId('fleet-enroll-key'), { target: { value: 'ssh-ed25519 AAAA' } });
    fireEvent.click(screen.getByTestId('fleet-enroll-submit'));
    const err = await screen.findByTestId('fleet-enroll-error');
    expect(err).toHaveTextContent('agent 2c8f6e1a9b47 is enrolled (serial 6 is not revoked): revoke it first');
    expect(screen.getByTestId('fleet-enroll-error-code')).toHaveTextContent('enrolled');
    expect(err).toHaveTextContent('Nothing was issued.');
    fireEvent.click(screen.getByTestId('fleet-enroll-revoke'));
    expect(await screen.findByTestId('fleet-revoke-confirm')).toHaveTextContent("Revoke 2c8f6e1a9b47's certificate");
  });

  it('revokes from a row menu with POST /fleet/revoke {agent_id, reason}', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/fleet/members': { body: { members: rows } },
      'POST /api/v1/fleet/revoke': { body: { agent_id: '91ab3f5c2d08', serial: '5', published: true } },
    });
    const onChanged = mount();
    // Own row and a revoked row have no menu.
    await screen.findAllByTestId('fleet-member-row');
    expect(screen.queryByTestId('fleet-member-menu-a3f9c0d12e7b')).toBeNull();
    expect(screen.queryByTestId('fleet-member-menu-e04d7c93b1f2')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-member-menu-91ab3f5c2d08'));
    fireEvent.click(screen.getByTestId('fleet-member-revoke'));
    const confirm = screen.getByTestId('fleet-revoke-confirm');
    expect(confirm).toHaveTextContent("Revoke 91ab3f5c2d08's certificate (tom-desktop.acme.example)?");
    expect(confirm).toHaveTextContent('To readmit it later, enroll it again with the same key.');
    fireEvent.change(screen.getByTestId('fleet-revoke-reason'), { target: { value: 'machine reimaged' } });
    fireEvent.click(screen.getByTestId('fleet-revoke-submit'));
    await waitFor(() => expect(calls.find(c => c.route === 'POST /api/v1/fleet/revoke')?.body)
      .toEqual({ agent_id: '91ab3f5c2d08', reason: 'machine reimaged' }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  // SABOTAGE: offer Enroll and the row menu on `master` alone -> red.
  it('over TCP (local false) offers no Enroll and no row menu, and says why on the master', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/members': { body: { members: rows } } });
    mount({ ...master, local: false });
    await screen.findAllByTestId('fleet-member-row');
    expect(screen.queryByTestId('fleet-enroll-open')).toBeNull();
    expect(screen.queryByRole('button', { name: /^Actions for/ })).toBeNull();
    expect(screen.getByTestId('fleet-members-desktop-only')).toHaveTextContent('Enroll and revoke need the desktop app');
  });

  it('a member sees the rows, no controls and no desktop note', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/members': { body: { members: rows } } });
    mount({ ...master, master: false, agent_id: '91ab3f5c2d08' });
    const table = await screen.findAllByTestId('fleet-member-row');
    expect(table).toHaveLength(4);
    expect(screen.queryByTestId('fleet-enroll-open')).toBeNull();
    expect(screen.queryByTestId('fleet-members-desktop-only')).toBeNull();
    expect(within(screen.getByTestId('fleet-members')).getByText(/Only the fleet's master enrolls, revokes and merges/)).toBeInTheDocument();
  });
});
