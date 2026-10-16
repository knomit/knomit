/// <reference types="node" />
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FleetFollowFlow } from './FleetFollowFlow';
import { OutcomeCard } from './OutcomeCard';
import { Refusal } from './fleetParts';
import { wizardErr } from './manageStyles';
import { gridClass, railClass, grid } from './connectStyles';
import { sameFleetURL } from './fleetText';
import { readFileSync } from 'node:fs';

// jsdom does not lay out, so the phone-width stack is pinned where it lives:
// the shared class names on the Connect layout and the media rule in App.css.

// process.cwd() is web/ under `npm test`; a bundler-processed ?raw import of a
// stylesheet comes back empty under vitest.
const css = readFileSync('src/App.css', 'utf8');

describe('Connect layout at phone width', () => {
  // SABOTAGE: drop the @media rule, or the className on the grid/rail -> red.
  it('stacks the rail above the form below 640px, in the shared style', () => {
    const media = /@media \(max-width: 640px\) \{([\s\S]*?)\n\}/.exec(css);
    expect(media).not.toBeNull();
    expect(media![1]).toMatch(new RegExp(`\\.${gridClass}\\s*\\{[^}]*grid-template-columns:\\s*minmax\\(0, 1fr\\);`));
    expect(media![1]).toMatch(new RegExp(`\\.${railClass}\\s*\\{[^}]*order:\\s*-1`));
    expect(grid.gridTemplateColumns).toBeUndefined();
  });
  it('the fleet follow flow uses the shared grid and rail classes', () => {
    render(<FleetFollowFlow fleetRepo="fleet" onCancel={() => {}} onDone={() => {}} />);
    const flow = screen.getByTestId('fleet-follow-flow');
    expect(flow.querySelector(`.${gridClass}`)).not.toBeNull();
    expect(flow.querySelector(`nav.${railClass}`)).not.toBeNull();
  });
});

describe('long URLs in refusal and outcome cards', () => {
  const long = 'file:///Users/someone/data/knomit/a/very/long/path/with/no/spaces/at/all/fleet-repository-123456789.git';
  // SABOTAGE: remove overflowWrap from wizardErr -> red.
  it('wraps a URL in the refusal box and the outcome card text', () => {
    expect(wizardErr.overflowWrap).toBe('anywhere');
    render(<><Refusal err={new Error(`cannot reach ${long}`)} /><OutcomeCard tone="bad" headline={`Couldn't reach ${long}`} url={long} body={long} /></>);
    const card = screen.getByTestId('outcome-card');
    expect((card.firstElementChild!.lastElementChild as HTMLElement).style.overflowWrap).toBe('anywhere');
    expect(card.style.minWidth).toBe('0px');
  });
});

describe('sameFleetURL mirrors the server comparison', () => {
  it('ignores credentials, case of scheme/host, default port and a trailing slash; keeps the path exact', () => {
    expect(sameFleetURL('https://u:t@GitHub.com:443/acme/fleet/', 'https://github.com/acme/fleet')).toBe(true);
    expect(sameFleetURL('https://github.com:8443/acme/fleet', 'https://github.com/acme/fleet')).toBe(false);
    expect(sameFleetURL('https://github.com/acme/Fleet', 'https://github.com/acme/fleet')).toBe(false);
    expect(sameFleetURL('git@github.com:acme/fleet.git', 'git@github.com:acme/fleet.git/')).toBe(true);
  });
});
