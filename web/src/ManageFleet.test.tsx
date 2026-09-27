import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { ManageFleet, registerBlocked, unregisterBlocked } from './ManageFleet';
import { api, type FleetStatus } from './api';

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    getFleet: vi.fn(),
    registerFleet: vi.fn(),
    unregisterFleet: vi.fn(),
    listFleetMembers: vi.fn(),
  },
}));

const standalone: FleetStatus = { state: 'standalone', agent_id: 'mindev-local-8ef0cd32', since: '2026-09-27T10:00:00Z' };
const unregistering: FleetStatus = {
  state: 'unregistering',
  agent_id: 'mindev-local-8ef0cd32',
  fleet_repo: 'fleet',
  fleet_url: 'https://github.com/knomit/fleet.git',
  record_state: 'active',
  since: '2026-09-27T10:00:00Z',
  last_attempt: '2026-09-27T10:05:00Z',
  last_error: 'dial tcp 140.82.121.4:443: connect: network is unreachable',
};

beforeEach(() => {
  vi.mocked(api.getFleet).mockResolvedValue(standalone);
  vi.mocked(api.listFleetMembers).mockResolvedValue([]);
});
afterEach(() => vi.clearAllMocks());

describe('the state machine decides what is allowed, and says why', () => {
  it('standalone: register allowed, unregister blocked', () => {
    expect(registerBlocked(standalone)).toBe('');
    expect(unregisterBlocked(standalone)).toMatch(/standalone/);
  });
  it('registering, registered and unregistering each block register with a reason', () => {
    for (const state of ['registering', 'registered', 'unregistering'] as const) {
      expect(registerBlocked({ ...standalone, state })).not.toBe('');
    }
  });
  it('unregistering blocks a second unregister', () => {
    expect(unregisterBlocked(unregistering)).toMatch(/already being pushed/);
    expect(unregisterBlocked({ ...unregistering, state: 'registered' })).toBe('');
  });
});

describe('ManageFleet', () => {
  it('shows the state, the record state and last_error verbatim, with the retry status', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(unregistering);
    render(<ManageFleet />);
    await waitFor(() => expect(screen.getByTestId('fleet-state').textContent).toBe('unregistering'));
    expect(screen.getByTestId('fleet-record-state').textContent).toContain('active');
    const errBox = screen.getByTestId('fleet-last-error');
    expect(errBox.textContent).toContain('dial tcp 140.82.121.4:443: connect: network is unreachable');
    expect(errBox.textContent).toContain('Retrying on the next sync');
  });

  it('disables the forbidden actions with their reasons', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(unregistering);
    render(<ManageFleet />);
    await waitFor(() => expect(screen.getByTestId('fleet-register')).toBeDisabled());
    expect(screen.getByTestId('fleet-unregister')).toBeDisabled();
    expect(screen.getByTestId('fleet-register-why').textContent).toMatch(/unregistration is still being pushed/);
    expect(screen.getByTestId('fleet-unregister-why').textContent).toMatch(/already being pushed/);
  });

  it('registers with the typed URL and keeps the form filled when it fails', async () => {
    vi.mocked(api.registerFleet).mockRejectedValue(new Error('clone failed: repository not found'));
    render(<ManageFleet />);
    await waitFor(() => expect(screen.getByTestId('fleet-state').textContent).toBe('standalone'));
    const input = screen.getByLabelText('Fleet repository URL') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'https://github.com/knomit/fleet.git' } });
    fireEvent.click(screen.getByTestId('fleet-register'));
    await waitFor(() => expect(screen.getByTestId('fleet-error').textContent).toContain('repository not found'));
    expect(api.registerFleet).toHaveBeenCalledWith('https://github.com/knomit/fleet.git', '');
    expect(input.value).toBe('https://github.com/knomit/fleet.git');
  });
});
