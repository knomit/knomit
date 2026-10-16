import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { FleetPage, capabilitiesText } from './ManageFleet';
import { api, type FleetStatus } from './api';

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    getFleet: vi.fn(),
    listFleetMembers: vi.fn(),
  },
}));

const standalone: FleetStatus = { state: 'standalone', agent_id: 'mindev-local-8ef0cd32', since: '2026-09-27T10:00:00Z' };
const registering: FleetStatus = {
  state: 'registering',
  agent_id: 'mindev-local-8ef0cd32',
  fleet_repo: 'fleet',
  fleet_urls: ['https://github.com/knomit/fleet.git'],
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

describe('FleetPage', () => {
  it('shows the state, the record state and last_error verbatim, with the retry status', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(registering);
    render(<FleetPage />);
    await waitFor(() => expect(screen.getByTestId('fleet-state').textContent).toBe('registering'));
    expect(screen.getByTestId('fleet-record-state').textContent).toContain('active');
    const errBox = screen.getByTestId('fleet-last-error');
    expect(errBox.textContent).toContain('dial tcp 140.82.121.4:443: connect: network is unreachable');
    expect(errBox.textContent).toContain('Retrying on the next sync');
  });
});

describe('F10: addresses, capabilities and the own record', () => {
  const registered: FleetStatus = {
    state: 'registered',
    agent_id: 'h1v302',
    fleet_repo: 'fleet',
    record_state: 'active',
    host: { listener: '', external_addresses: ['https://h1v302.tail5113a7.ts.net'] },
    record: {
      addresses: ['https://h1v302.tail5113a7.ts.net'],
      git: '/git',
      capabilities: { os: 'darwin', arch: 'arm64', version: '0.5.0.abc', read_only: 'false' },
    },
    record_current: true,
    record_pending_update: true,
  };

  it('capabilitiesText puts os/arch and version first and shows the rest as written', () => {
    expect(capabilitiesText({ version: '0.5.0', arch: 'amd64', os: 'linux', read_only: 'false', odd: '' })).toBe(
      'linux/amd64 · 0.5.0 · odd · read_only=false',
    );
    expect(capabilitiesText(undefined)).toBe('');
  });

  it('shows the configured addresses, the own record and "pending update"', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(registered);
    render(<FleetPage />);
    await waitFor(() => expect(screen.getByTestId('fleet-state').textContent).toBe('registered'));
    expect(screen.getByTestId('fleet-external-addresses').textContent).toBe('https://h1v302.tail5113a7.ts.net');
    expect(screen.getByTestId('fleet-own-record').textContent).toContain('darwin/arm64 · 0.5.0.abc · read_only=false');
    expect(screen.getByTestId('fleet-record-sync').textContent).toMatch(/^pending update/);
    expect(screen.queryByTestId('fleet-notice')).toBeNull();
  });

  it('says when the record differs from the config, and shows the notice with no addresses', async () => {
    vi.mocked(api.getFleet).mockResolvedValue({
      ...registered,
      host: { listener: '', external_addresses: [] },
      record_current: false,
      notice: 'no external addresses configured; set `external_addresses` in knomit.toml',
    });
    render(<FleetPage />);
    await waitFor(() => expect(screen.getByTestId('fleet-notice').textContent).toContain('external_addresses'));
    expect(screen.getByTestId('fleet-external-addresses').textContent).toBe('none');
    expect(screen.getByTestId('fleet-record-sync').textContent).toMatch(/restart to update it/);
  });

  it('lists every member with its addresses, capabilities, host and state as written', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(registered);
    vi.mocked(api.listFleetMembers).mockResolvedValue([
      { agent: 'h1v302', state: 'active', host: 'h1v302', addresses: ['https://h1v302.tail5113a7.ts.net'], capabilities: { os: 'darwin', arch: 'arm64', version: '0.5.0.abc' } },
      { agent: 'peer', state: 'left', host: 'box', addresses: ['not-a-url'], capabilities: { os: 'plan9' } },
    ]);
    render(<FleetPage />);
    await waitFor(() => expect(screen.getAllByTestId('fleet-member')).toHaveLength(2));
    const [a, b] = screen.getAllByTestId('fleet-member');
    expect(a.textContent).toContain('https://h1v302.tail5113a7.ts.net');
    expect(a.textContent).toContain('darwin/arm64 · 0.5.0.abc');
    expect(b.textContent).toContain('left');
    expect(b.textContent).toContain('box');
    expect(b.textContent).toContain('not-a-url');
    expect(b.textContent).toContain('plan9/?');
  });
});

// The page is the index + blocks layout: This instance, then Members. No
// action block: every fleet operation is a local-listener route a browser
// page cannot reach (POST /api/v1/fleet/join, /leave, ...).
describe('the Fleet page layout', () => {
  const sectionIds = () => Array.from(screen.getByTestId('fleet-page').querySelectorAll('section')).map(s => s.id);
  const registered: FleetStatus = { state: 'registered', agent_id: 'a1', fleet_repo: 'fleet', record_state: 'active' };

  it('a registered instance and a standalone one show the same two blocks', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(registered);
    const { unmount } = render(<FleetPage />);
    await waitFor(() => expect(screen.getByTestId('fleet-state').textContent).toBe('registered'));
    expect(sectionIds()).toEqual(['this-instance', 'members']);
    unmount();
    vi.mocked(api.getFleet).mockResolvedValue(standalone);
    render(<FleetPage />);
    await waitFor(() => expect(screen.getByTestId('fleet-state').textContent).toBe('standalone'));
    expect(sectionIds()).toEqual(['this-instance', 'members']);
  });

  it('indexes the blocks under a "Fleet" heading', async () => {
    vi.mocked(api.getFleet).mockResolvedValue(registered);
    render(<FleetPage />);
    await waitFor(() => expect(sectionIds()).toEqual(['this-instance', 'members']));
    expect(screen.getByTestId('settings-index-heading')).toHaveTextContent('Fleet');
    expect(screen.getByTestId('toc-this-instance')).toHaveTextContent('This instance');
    expect(screen.getByTestId('toc-members')).toHaveTextContent('Members');
  });
});

// Operators pass this instance's agent id to the master's enroll, so the id
// the page shows can be copied as-is: exactly the server's opaque string.
describe('agent id copy control', () => {
  it('sits next to the agent id and copies exactly that id to the clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    vi.mocked(api.getFleet).mockResolvedValue({ ...standalone, agent_id: 'opaque id/with:odd-chars 7' });
    render(<FleetPage />);
    const btn = await screen.findByTestId('fleet-agent-id-copy');
    expect(btn.tagName).toBe('BUTTON');
    expect(btn.querySelector('svg')).not.toBeNull();
    expect(btn.parentElement?.textContent).toBe('opaque id/with:odd-chars 7');
    fireEvent.click(btn);
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith('opaque id/with:odd-chars 7');
  });
});
