import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FleetSettings } from './FleetSettings';
import { FleetWizard } from './FleetWizard';
import { FacetLayoutProvider } from './facetLayout';
import type { FleetStatus } from './api';
import { stubFleetFetch } from './fleetTestFetch';

// Stored secrets on the fleet's forge flows: Update access, New fleet
// (create) and join send secret_id and no pasted credential.

const FORGE = 'https://github.com/acme/fleet';
const SECRETS = { body: { secrets: [{ id: 's1', name: 'gh-pat', type: 'token', hosts: ['github.com'], expires_at: null, status: 'expired', created_at: '', updated_at: '', linked_repos: 0 }] } };
const PROBE_OK = { body: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] } };
const PROBE_ASKS = { body: { reachable: true, empty: false, auth_required: true, upstream_branch: '', branches: [] } };
const forgeMember: FleetStatus = {
  state: 'registered', agent_id: '5be2d8f4a901', fleet_repo: 'fleet', master: false, local: true, hosting: FORGE,
  record_state: 'active', public_key: 'ssh-ed25519 AAAA', root_dir: '/home/k/fleet', root_fingerprint: 'SHA256:root',
  host: { listener_on: false, external_addresses: [] }, fleet_urls: [FORGE], host_record: [FORGE], follows_host: true,
};
const standalone: FleetStatus = {
  state: 'standalone', agent_id: '5be2d8f4a901', public_key: 'ssh-ed25519 AAAAjoiner', local: true, root_dir: '/home/k/fleet',
  host: { listener_on: true, external_addresses: ['https://kb-01.acme.example:7443'], would_serve: ['https://kb-01.acme.example:7443/git/fleet'] },
  fleet_urls: [],
};

afterEach(() => { vi.unstubAllGlobals(); });

describe('fleet: stored secrets', () => {
  it('Update access with a stored secret posts /fleet/follow {secret_id}', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token' } },
      'GET /api/v1/secrets': SECRETS,
      'POST /api/v1/repos:probe-origin': PROBE_OK,
      'POST /api/v1/fleet/follow': { body: { ...forgeMember, change: 'credential' } },
    });
    const onChanged = vi.fn();
    render(<FleetSettings fleet={forgeMember} onChanged={onChanged} />);
    await screen.findByTestId('fleet-origin-access');
    fireEvent.click(screen.getByTestId('fleet-update-access'));
    fireEvent.change(screen.getByTestId('fleet-follow-auth-method'), { target: { value: 'secret' } });
    expect(screen.queryByTestId('fleet-follow-auth-token')).toBeNull();
    expect(screen.getByTestId('fleet-follow-next')).toBeDisabled();
    fireEvent.change(await screen.findByTestId('fleet-follow-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('fleet-follow-next'));
    fireEvent.click(screen.getByTestId('fleet-follow-submit'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/follow')?.body).toEqual({ secret_id: 's1' });
  });

  it('a linked fleet origin names its secret and expiry in the Access row', async () => {
    stubFleetFetch({
      'GET /api/v1/repos/fleet/origin': { body: { name: 'origin', url: FORGE, branch: 'main', auth_method: 'token', secret_name: 'gh-pat', secret_status: 'expired' } },
    });
    render(<FleetSettings fleet={forgeMember} onChanged={() => {}} />);
    await waitFor(() => expect(screen.getByTestId('fleet-origin-access')).toHaveTextContent('stored secret: gh-pat'));
    expect(screen.getByTestId('fleet-origin-secret-chip')).toHaveTextContent('expired');
  });

  it('New fleet on a forge with a stored secret posts /fleet/create with secret_id and no token', async () => {
    const { calls } = stubFleetFetch({
      'POST /api/v1/repos:probe-origin': body => ((body as { secret_id?: string }).secret_id ? PROBE_OK : PROBE_ASKS),
      'GET /api/v1/secrets': SECRETS,
      'POST /api/v1/fleet/create': { status: 202, body: { ...standalone, state: 'registering', fleet_repo: 'fleet' } },
    });
    const onDone = vi.fn();
    render(<FacetLayoutProvider value={{ width: 220, onSplitterDown: () => {} } as never}><FleetWizard fleet={standalone} onDone={onDone} onCancel={() => {}} /></FacetLayoutProvider>);
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-hosting-forge'));
    fireEvent.change(screen.getByTestId('fleet-forge-url'), { target: { value: FORGE } });
    fireEvent.click(screen.getByTestId('fleet-forge-connect'));
    await screen.findByTestId('fleet-step-access');
    fireEvent.change(screen.getByTestId('fleet-auth-method'), { target: { value: 'secret' } });
    fireEvent.change(await screen.findByTestId('fleet-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('fleet-access-check'));
    await screen.findByTestId('fleet-access-ok');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/create')?.body).toEqual({ origin: { url: FORGE, secret_id: 's1' } });
  });

  it('join on a forge with a stored secret posts secret_id', async () => {
    const BUNDLE = '# knomit fleet bundle v1\nurl: https://github.com/acme/fleet.git\n';
    const forge = { url: 'https://github.com/acme/fleet.git', hosting: 'forge', host: 'github.com', root_fingerprint: 'SHA256:root', agent_id: '5be2d8f4a901', auth_required: true };
    const { calls } = stubFleetFetch({
      'POST /api/v1/fleet/join:check': body => ({ body: (body as { secret_id?: string }).secret_id ? { ...forge, auth_required: false } : forge }),
      'GET /api/v1/secrets': SECRETS,
      'POST /api/v1/fleet/join': { body: { ...standalone, state: 'registered', root_fingerprint: 'SHA256:root' } },
    });
    const onDone = vi.fn();
    render(<FacetLayoutProvider value={{ width: 220, onSplitterDown: () => {} } as never}><FleetWizard fleet={standalone} onDone={onDone} onCancel={() => {}} /></FacetLayoutProvider>);
    fireEvent.click(screen.getByTestId('fleet-choice-join'));
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    Object.defineProperty(navigator, 'clipboard', { value: { readText: vi.fn().mockResolvedValue(BUNDLE), writeText: vi.fn() }, configurable: true });
    fireEvent.click(screen.getByTestId('fleet-bundle-paste'));
    fireEvent.click(await screen.findByTestId('fleet-bundle-check'));
    await screen.findByTestId('fleet-step-access');
    fireEvent.change(screen.getByTestId('fleet-auth-method'), { target: { value: 'secret' } });
    fireEvent.change(await screen.findByTestId('fleet-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('fleet-access-check'));
    await screen.findByTestId('fleet-access-ok');
    fireEvent.click(screen.getByTestId('fleet-wizard-next'));
    fireEvent.click(screen.getByTestId('fleet-wizard-primary'));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(calls.find(c => c.route === 'POST /api/v1/fleet/join')?.body).toEqual({ bundle: BUNDLE, secret_id: 's1' });
  });
});
