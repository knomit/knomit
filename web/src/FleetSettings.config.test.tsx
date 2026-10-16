import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetSettings } from './FleetSettings';
import type { FleetStatus } from './api';
import { stubFleetFetch, problem } from './fleetTestFetch';
import { hostPortOf } from './fleetText';

// PR7c: the fleet repository's configuration lives under /fleet (user
// rulings 2026-10-16). Move the fleet to a forge (the hosting master), Update
// access (a forge copy), Point at the new host after the fleet moved to a
// forge (the member's own credential), and Index → Rebuild. Every change is
// POST /fleet/..., never a generic repo route.

const FORGE = 'https://github.com/acme/fleet';
const hub: FleetStatus = {
  state: 'registered', agent_id: 'a3f9c0d12e7b', fleet_repo: 'fleet', master: true, local: true, hosting: 'none',
  record_state: 'active', public_key: 'ssh-ed25519 AAAAmaster', root_dir: '/home/k/fleet', root_fingerprint: 'SHA256:root',
  host: { listener_on: true, listening: true, external_addresses: ['https://kb-01.acme.example:7443'] },
  fleet_urls: ['https://kb-01.acme.example:7443/git/fleet'], follows_host: true,
};
const forgeMember: FleetStatus = {
  ...hub, agent_id: '5be2d8f4a901', master: false, hosting: FORGE, fleet_urls: [FORGE], host_record: [FORGE],
  host: { listener_on: false, external_addresses: [] },
};
const PROBE_FORGE_ASKS = { body: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: [] } };
const PROBE_FORGE_OK = { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] } };
const PROBE_PEER = { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], fleet_peer: true } };

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function mount(fleet: FleetStatus) {
  const onChanged = vi.fn();
  render(<FleetSettings fleet={fleet} onChanged={onChanged} />);
  return onChanged;
}

describe('hostPortOf (#528 N6)', () => {
  // SABOTAGE: return the host alone (hostOf) -> the :19151 case is red.
  it('keeps a non-default port and drops the default one', () => {
    expect(hostPortOf('https://127.0.0.1:19151/git/fleet')).toBe('127.0.0.1:19151');
    expect(hostPortOf('https://u:tok@kb.example:443/git/fleet')).toBe('kb.example');
    expect(hostPortOf('ssh://git@github.com:2222/acme/fleet.git')).toBe('github.com:2222');
    expect(hostPortOf('git@github.com:acme/fleet.git')).toBe('github.com');
    expect(hostPortOf('file:///srv/fleet.git')).toBe('');
  });
});

describe('FleetSettings: the fleet repository configuration (PR7c)', () => {
  // The hosting master's Move: a forge URL, the Access step (the forge asks),
  // its credential checked, then a Review whose list says what members must
  // do, and POST /fleet/follow {url, credential}.
  // SABOTAGE: drop the member consequence item -> red; send the token as a
  // PUT /repos/fleet/origin -> no /fleet/follow call -> red.
  it('the hub master moves the fleet to a forge through the Access step and a Review naming the members\' step', async () => {
    let probes = 0;
    const { calls } = stubFleetFetch({
      'POST /api/v1/repos:probe-origin': () => (probes++ === 0 ? PROBE_FORGE_ASKS : PROBE_FORGE_OK),
      'POST /api/v1/fleet/follow': { body: { ...hub, hosting: FORGE } },
    });
    const onChanged = mount(hub);
    expect(screen.getByTestId('fleet-move-forge-hint')).toHaveTextContent('This instance stays the master');
    fireEvent.click(screen.getByTestId('fleet-move-forge'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveAttribute('data-mode', 'move');
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: FORGE } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-access-ask')).toHaveTextContent('github.com wants credentials');
    expect(screen.getByTestId('fleet-follow-next')).toBeDisabled();
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'token' } });
    fireEvent.change(screen.getByTestId('fleet-follow-auth-token'), { target: { value: 'ghp_new' } });
    fireEvent.click(screen.getByTestId('fleet-follow-check'));
    expect(await screen.findByTestId('fleet-follow-access-ok')).toBeInTheDocument();
    expect(calls.filter(c => c.route === 'POST /api/v1/repos:probe-origin')[1].body).toEqual({ url: FORGE, auth_method: 'token', auth_token: 'ghp_new' });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    const members = screen.getByTestId('fleet-move-members');
    expect(members).toHaveTextContent("Every member's pushes to this instance are refused from then on, until it points at " + FORGE + ' with its own credential');
    expect(members).toHaveTextContent('Its unmerged work is not lost');
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: FORGE, auth_method: 'token', auth_token: 'ghp_new' });
    expect(calls.map(c => c.route).filter(r => r.includes('/repos/fleet'))).toEqual([]);
  });

  // SABOTAGE: drop the peerForMove card -> the flow goes on to Review -> red.
  it('Move refuses an instance of this fleet: that is a master switch, not a forge', async () => {
    const { calls } = stubFleetFetch({ 'POST /api/v1/repos:probe-origin': PROBE_PEER });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-move-forge'));
    fireEvent.change(screen.getByTestId('fleet-follow-url'), { target: { value: 'https://kb-02.acme.example:7443/git/fleet' } });
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-peer-not-forge')).toHaveTextContent('kb-02.acme.example:7443 is an instance of this fleet, not a forge');
    expect(screen.queryByTestId('fleet-follow-submit')).toBeNull();
    expect(calls.map(c => c.route)).not.toContain('POST /api/v1/fleet/follow');
  });

  // A forge copy: its stored method shown, Update access prefilled with the
  // stored URL and method; "none" resets it to anonymous. A refusal shows the
  // server's words and code.
  // SABOTAGE: prefill nothing -> the URL field is empty -> red.
  it('a forge member updates its access: prefilled, the stored method named, POST /fleet/follow with the new method', async () => {
    const { calls, set } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
      'POST /api/v1/fleet/follow': problem(502, 'unreachable', 'the remote refused the credential'),
    });
    const onChanged = mount(forgeMember);
    expect(await screen.findByTestId('fleet-origin-access')).toHaveTextContent('token');
    expect(screen.queryByTestId('fleet-point-at')).toBeNull();
    expect(screen.queryByTestId('fleet-move-forge')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-update-access'));
    expect(screen.getByTestId('fleet-follow-flow')).toHaveAttribute('data-mode', 'access');
    expect(screen.getByTestId('fleet-follow-url')).toHaveValue(FORGE);
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-access')).toHaveTextContent('Stored now: token');
    expect(screen.getByTestId('fleet-follow-auth-method')).toHaveValue('token');
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'none' } });
    expect(screen.queryByTestId('fleet-follow-auth-token')).toBeNull();
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    expect(await screen.findByTestId('fleet-follow-error-code')).toHaveTextContent('unreachable');
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: FORGE, auth_method: 'none' });
    expect(onChanged).not.toHaveBeenCalled();
    set('POST /api/v1/fleet/follow', { body: forgeMember });
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  // After the hub moved to a forge: a member still on the old hub (a cert
  // origin) sees the stale card with the honest consequence, and Point at is
  // prefilled with the forge from main's host record and asks for its own
  // credential.
  // SABOTAGE: skip the Access step for a non-peer -> no credential fields ->
  // red.
  it('a member left on the old hub points at the forge with its own credential', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: 'https://kb-01.acme.example:7443/git/fleet', branch: 'main', auth_method: 'cert' } },
      'POST /api/v1/repos:probe-origin': PROBE_FORGE_OK,
      'POST /api/v1/fleet/follow': { body: forgeMember },
    });
    const onChanged = mount({ ...forgeMember, hosting: 'https://kb-01.acme.example:7443/git/fleet', follows_host: false });
    const card = await screen.findByTestId('fleet-follows-old-host');
    expect(card).toHaveTextContent('Until then the old host refuses this copy\'s pushes; nothing it wrote is lost');
    expect(screen.getByTestId('fleet-point-at-hint')).toHaveTextContent("On a forge it asks for this instance's own credential");
    fireEvent.click(screen.getByTestId('fleet-point-at'));
    expect(screen.getByTestId('fleet-follow-url')).toHaveValue(FORGE);
    expect(screen.getByTestId('fleet-follow-host-record')).toHaveTextContent(FORGE);
    fireEvent.click(screen.getByTestId('fleet-follow-test'));
    expect(await screen.findByTestId('fleet-follow-access')).toBeInTheDocument();
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'basic' } });
    fireEvent.change(screen.getByTestId('fleet-follow-auth-user'), { target: { value: 'bravo' } });
    fireEvent.change(screen.getByTestId('fleet-follow-auth-token'), { target: { value: 'pw' } });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    expect(screen.getByTestId('fleet-follow-will')).toHaveTextContent("carries this fleet's root");
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ url: FORGE, auth_method: 'basic', auth_token: 'bravo:pw' });
  });

  // Index → Rebuild: POST /fleet/index-rebuild, the started line; a refusal
  // under it. Over TCP (local false) there is no button (the TCP test in
  // FleetSettings.test.tsx asserts no action button at all).
  // SABOTAGE: call api.rebuild (the generic route) -> no /fleet call -> red.
  it('Index → Rebuild starts the fleet repository\'s rebuild through /fleet', async () => {
    const { calls, set } = stubFleetFetch({
      'POST /api/v1/fleet/index-rebuild': { status: 201, body: { id: 'rebuild-1', kind: 'index-rebuild', state: 'running', repo: 'fleet', branch: 'agent/a3f9c0d12e7b' } },
    });
    mount(hub);
    fireEvent.click(screen.getByTestId('fleet-rebuild'));
    expect(await screen.findByTestId('fleet-rebuild-status')).toHaveTextContent('Rebuild started');
    expect(calls.map(c => c.route)).toContain('POST /api/v1/fleet/index-rebuild');
    expect(calls.map(c => c.route).filter(r => r.includes('index-rebuilds'))).toEqual([]);
    set('POST /api/v1/fleet/index-rebuild', problem(409, 'not_registered', 'this instance is in no fleet'));
    fireEvent.click(screen.getByTestId('fleet-rebuild'));
    expect(await screen.findByTestId('fleet-rebuild-error-code')).toHaveTextContent('not_registered');
  });
});
