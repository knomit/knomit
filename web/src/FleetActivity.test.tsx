import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FleetActivity } from './FleetActivity';
import type { FleetEvent, FleetStatus } from './api';
import { stubFleetFetch } from './fleetTestFetch';

const fleet = { state: 'registered', agent_id: 'a3f9c0d12e7b', fleet_repo: 'fleet', master: true, local: true,
  host: { listener_on: true, external_addresses: [] }, fleet_urls: [] } as FleetStatus;
const events: FleetEvent[] = [
  { at: '2026-10-16T09:00:00Z', kind: 'refused', agent: 'c71e4ab60d3f', branch: 'agent/c71e4ab60d3f', tip: '9e21c0f1234', detail: 'not the branch owner' },
  { at: '2026-10-15T09:00:00Z', kind: 'revoked', agent: 'e04d7c93b1f2', serial: '4', detail: 'machine reimaged' },
  { at: '2026-10-14T09:00:00Z', kind: 'host-changed', agent: 'a3f9c0d12e7b', detail: 'hosted at https://kb-01.acme.example:7443/git/fleet' },
  { at: '2026-10-13T09:00:00Z', kind: 'admitted', agent: '5be2d8f4a901', branch: 'agent/5be2d8f4a901', tip: '4b1e2a0', detail: 'agent/5be2d8f4a901 @ 4b1e2a0 merged into main' },
  { at: '2026-10-12T09:00:00Z', kind: 'created', agent: 'a3f9c0d12e7b', detail: 'fleet created' },
];

afterEach(() => { vi.unstubAllGlobals(); });

describe('FleetActivity', () => {
  it('reads GET /fleet/activity and draws each event with its name, counts and detail', async () => {
    const { calls } = stubFleetFetch({
      'GET /api/v1/fleet/activity': { body: { events } },
      'GET /api/v1/fleet/members': { body: { members: [{ agent: '5be2d8f4a901', state: 'active', host: 'ada-laptop.acme.example' }] } },
    });
    render(<FleetActivity fleet={fleet} />);
    const rows = await screen.findAllByTestId('fleet-event');
    expect(calls.map(c => c.route)).toContain('GET /api/v1/fleet/activity');
    expect(rows.map(r => r.getAttribute('data-kind'))).toEqual(['refused', 'revoked', 'host-changed', 'admitted', 'created']);
    expect(screen.getByTestId('fleet-activity-counts')).toHaveTextContent('5 events · 1 refusals');
    expect(rows[2]).toHaveTextContent('Host changed');
    expect(rows[3]).toHaveTextContent('Admitted into main');
    expect(await screen.findByText('ada-laptop.acme.example')).toBeInTheDocument();
  });

  // #527 F2. SABOTAGE: drop the "claimed author" line -> red.
  it('a refused event names its agent as the claimed author, with the branch and tip', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/activity': { body: { events } }, 'GET /api/v1/fleet/members': { body: { members: [] } } });
    render(<FleetActivity fleet={fleet} />);
    const [refused] = await screen.findAllByTestId('fleet-event');
    expect(refused).toHaveTextContent('Branch refused');
    expect(refused).toHaveTextContent('c71e4ab60d3f');
    expect(refused).toHaveTextContent('claimed author');
    expect(refused).toHaveTextContent('agent/c71e4ab60d3f @ 9e21c0f: not the branch owner');
  });

  // Root-imported, stood-down and decommissioned are not events (#527).
  it('names no event the server does not keep', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/activity': { body: { events } }, 'GET /api/v1/fleet/members': { body: { members: [] } } });
    render(<FleetActivity fleet={fleet} />);
    await screen.findAllByTestId('fleet-event');
    const text = screen.getByTestId('fleet-activity').textContent ?? '';
    for (const gone of ['Root key imported', 'Stood down', 'Decommissioned']) expect(text).not.toContain(gone);
  });

  it('says so when nothing happened yet', async () => {
    stubFleetFetch({ 'GET /api/v1/fleet/activity': { body: { events: [] } }, 'GET /api/v1/fleet/members': { body: { members: [] } } });
    render(<FleetActivity fleet={fleet} />);
    expect(await screen.findByText('Nothing yet.')).toBeInTheDocument();
  });
});
