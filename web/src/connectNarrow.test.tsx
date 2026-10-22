/// <reference types="node" />
import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FleetFollowFlow } from './FleetFollowFlow';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { OutcomeCard } from './OutcomeCard';
import { Refusal } from './fleetParts';
import { gridClass, railClass, grid } from './connectStyles';
import { sameFleetURL } from './fleetText';
import { readFileSync } from 'node:fs';
import { computed } from './cascade.testutil';

vi.mock('./api', async importOriginal => ({
  RepoIndexingError: (await importOriginal<typeof import('./api')>()).RepoIndexingError,
  api: {
    getOrigin: vi.fn().mockResolvedValue(null),
    listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
  },
  createSession: vi.fn(), streamTest: vi.fn(), streamPreview: vi.fn(), streamApply: vi.fn(), streamCommit: vi.fn(),
  deleteSession: vi.fn().mockResolvedValue(undefined),
}));

const TWO_COLUMNS = 'minmax(0, 1fr) 178px';
const STACKED = 'minmax(0, 1fr)';

// Both pages of the Connect layout, each as the page itself renders it.
const pages: Array<[string, () => void, () => HTMLElement]> = [
  ['repo Connect (RemoteConnectWizard)', () => { render(<RemoteConnectWizard repo="knomit" onCancel={() => {}} onDone={() => {}} />); }, () => document.body],
  ['fleet Connect (FleetFollowFlow)', () => { render(<FleetFollowFlow fleetRepo="fleet" onCancel={() => {}} onDone={() => {}} />); }, () => screen.getByTestId('fleet-follow-flow')],
];

describe.each(pages)('Connect layout, %s', (_name, mount, root) => {
  // SABOTAGE: drop className={gridClass} / className={railClass} from the page -> red.
  it('carries the shared grid and rail classes, and no two-column template inline', () => {
    mount();
    const grids = root().querySelectorAll(`.${gridClass}`);
    expect(grids).toHaveLength(1);
    expect(grids[0].querySelector(`:scope > nav.${railClass}`)).not.toBeNull();
    // The template lives in the class (an inline style cannot hold a media query).
    expect(grid.gridTemplateColumns).toBeUndefined();
    expect((grids[0] as HTMLElement).style.getPropertyValue('grid-template-columns')).toBe('');
  });

  // SABOTAGE: repeat the base .k-connect-grid rule after the @media block; drop
  // `position: static !important` or `order: -1` from the stacked rail; change
  // the breakpoint -> red.
  it('keeps two columns and a sticky rail wide, and stacks the rail above the form at phone width', () => {
    mount();
    const g = root().querySelector(`.${gridClass}`)!;
    const rail = g.querySelector(`:scope > nav.${railClass}`)!;
    for (const width of [1280, 641]) {
      expect(computed(g, 'grid-template-columns', width), `grid at ${width}`).toBe(TWO_COLUMNS);
      expect(computed(rail, 'position', width), `rail position at ${width}`).toBe('sticky');
      expect(computed(rail, 'order', width), `rail order at ${width}`).toBe('');
    }
    for (const width of [640, 375]) {
      expect(computed(g, 'grid-template-columns', width), `grid at ${width}`).toBe(STACKED);
      expect(computed(rail, 'position', width), `rail position at ${width}`).toBe('static');
      expect(computed(rail, 'order', width), `rail order at ${width}`).toBe('-1');
    }
  });
});

describe('long URLs in refusal and outcome cards', () => {
  const long = 'file:///Users/someone/data/knomit/a/very/long/path/with/no/spaces/at/all/fleet-repository-123456789.git';
  // SABOTAGE: remove overflowWrap from wizardErr, or from OutcomeCard's headline
  // or body, or wordBreak from its url line, or minWidth from its box -> red.
  it('wraps a URL in the rendered refusal box and the rendered outcome card', () => {
    render(<><Refusal err={new Error(`cannot reach ${long}`)} /><OutcomeCard tone="bad" headline={`Couldn't reach ${long}`} url={long} body={long} /></>);
    expect(screen.getByTestId('fleet-refusal').style.overflowWrap).toBe('anywhere');
    const card = screen.getByTestId('outcome-card');
    expect(card.style.minWidth).toBe('0px');
    const [head, url, body] = Array.from(card.children) as HTMLElement[];
    expect((head.lastElementChild as HTMLElement).style.overflowWrap).toBe('anywhere');
    expect(url).toHaveTextContent(long);
    expect(url.style.wordBreak).toBe('break-all');
    expect(body).toHaveTextContent(long);
    expect(body.style.overflowWrap).toBe('anywhere');
  });
});

// The browser's port of the server's comparison is held to the SAME table the
// Go test reads (internal/corpus/repos/testdata/same_fleet_url.json), the typed side
// trimmed as the page trims it and the server's followFleetHost does.
// SABOTAGE: replace the body of sameFleetURL with a === b -> the rows with
// credentials, case, default ports and trailing slashes are red.
describe('sameFleetURL mirrors the server comparison', () => {
  const rows = JSON.parse(readFileSync('../internal/corpus/repos/testdata/same_fleet_url.json', 'utf8')) as Array<{ typed: string; stored: string; same: boolean }>;
  it('reads the shared table', () => {
    expect(rows.length).toBeGreaterThanOrEqual(30);
    expect(rows.some(r => r.same)).toBe(true);
    expect(rows.some(r => !r.same)).toBe(true);
  });
  it.each(rows.map(r => [r.typed, r.stored, r.same] as const))('%j vs %j is %s', (typed, stored, same) => {
    expect(sameFleetURL(typed.trim(), stored)).toBe(same);
  });
});
