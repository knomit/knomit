// The "+" picker's presentation. The behaviour it wraps — fetch, drill, pick,
// close — is unchanged and covered elsewhere; these pin the layout decisions,
// because every one of them is the kind of thing a later style edit undoes
// without noticing.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup, act } from '@testing-library/react';

const completions = vi.fn();
vi.mock('./api', async () => {
  const actual = await vi.importActual<typeof import('./api')>('./api');
  return { ...actual, api: { completions: (...a: unknown[]) => completions(...a), lensCompletions: vi.fn() } };
});

import { FilterBar } from './FilterBar';
import { init } from './state';
import type { AppState } from './state';
import { typeStyles } from './utils';

const state: AppState = { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa' };

// The order the server actually hands back: SELECT DISTINCT with no ORDER BY.
const DOMAINS = ['agentic engineering', 'job state', 'context engineering', 'evaluation',
  'rag', 'tools', 'architecture', 'debugging', 'ai', 'security'];

function openPicker(values: string[] = DOMAINS) {
  completions.mockResolvedValue({ values });
  render(<FilterBar state={state} dispatch={vi.fn()} />);
  fireEvent.click(screen.getByTitle('Add filter'));
  return screen.getByTestId('filter-picker');
}

async function openCategory(label: string, values: string[] = DOMAINS) {
  const picker = openPicker(values);
  fireEvent.click(screen.getByTestId(`picker-cat-${label}`));
  await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBeGreaterThan(0));
  return picker;
}

const shown = () => screen.getAllByTestId('picker-value').map(e => e.textContent);

describe('FilterBar — the value picker', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('lays the values out in columns rather than one tall scroller', async () => {
    await openCategory('domain');
    const grid = screen.getByTestId('picker-values');
    expect(grid.style.display).toBe('grid');
    expect(grid.style.gridTemplateColumns).toContain('repeat(3');
  });

  it('sorts an open set alphabetically — the server returns no order at all', async () => {
    await openCategory('domain');
    expect(shown()).toEqual([...DOMAINS].sort((a, b) => a.localeCompare(b)));
    // The whole point: the API order buried "ai" ninth.
    expect(shown()[0]).toBe('agentic engineering');
    expect(shown()[1]).toBe('ai');
  });

  it('leaves a curated enum in the order the server chose', async () => {
    // Types arrive grouped epistemic-then-pragmatic, kinds and origins in a
    // deliberate order. Alphabetising would scramble a meaning that is already
    // there — only the unordered sets get sorted.
    const TYPES = ['observation', 'concept', 'process', 'principle', 'pattern'];
    await openCategory('type', TYPES);
    expect(shown()).toEqual(TYPES);
  });

  it('gives a type value the colour it wears everywhere else', async () => {
    await openCategory('type', ['observation', 'synthesis']);
    const row = screen.getAllByTestId('picker-value')[0];
    expect(row.style.color).toBe(hexToRgb(typeStyles.observation.color));
  });

  it('holds one height, so switching category or typing does not resize the panel', async () => {
    // A panel that grows and shrinks under the cursor moves the row you were
    // aiming at. FacetBrowser fixed its height for the same reason.
    await openCategory('domain');
    const tall = screen.getByTestId('picker-values').style.height;
    expect(tall).not.toBe('');
    fireEvent.change(screen.getByTestId('picker-search'), { target: { value: 'zzz' } });
    await waitFor(() => expect(screen.getByTestId('picker-values').style.height).toBe(tall));
  });

  it('hides the search field for a set small enough to read whole', async () => {
    await openCategory('kind', ['epistemic', 'pragmatic']);
    expect(screen.queryByTestId('picker-search')).toBeNull();
  });

  it('keeps the search field once something is typed, however few match', async () => {
    await openCategory('domain');
    fireEvent.change(screen.getByTestId('picker-search'), { target: { value: 'ai' } });
    completions.mockResolvedValue({ values: ['ai'] });
    await waitFor(() => expect(screen.getByTestId('picker-search')).toBeTruthy());
  });

  it('still adds the chip when a value is picked', async () => {
    completions.mockResolvedValue({ values: DOMAINS });
    const dispatch = vi.fn();
    render(<FilterBar state={state} dispatch={dispatch} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    fireEvent.click(screen.getByTestId('picker-cat-domain'));
    await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBeGreaterThan(0));
    fireEvent.mouseDown(screen.getAllByTestId('picker-value').find(e => e.textContent === 'ai')!);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'domain', value: 'ai' },
    });
  });

  it('keeps the path drill-down affordance', async () => {
    await openCategory('path', ['kb/decisions', 'kb/invariants']);
    expect(screen.getAllByTestId('picker-drill').length).toBe(2);
  });

  it('opens Path at the root CHILDREN, not at the root itself', async () => {
    // The tree's top level is one entry — the ontology root, which every fact
    // is under. Opening there spent a click and two empty columns to tell the
    // reader something they already knew.
    await openCategory('path', ['kb/architecture', 'kb/decisions', 'kb/invariants']);
    expect(completions).toHaveBeenCalledWith('core', 'agent/main', 'path', 'kb/');
    expect(shown()).toEqual(['architecture', 'decisions', 'invariants']);
  });

  it('offers no way up from the root level, because there is nothing above it', async () => {
    await openCategory('path', ['kb/architecture', 'kb/decisions']);
    expect(screen.queryByTestId('picker-up')).toBeNull();
  });

  it('shows the crumb once you drill, and climbs back to the root children', async () => {
    await openCategory('path', ['kb/architecture', 'kb/decisions']);
    completions.mockResolvedValue({ values: ['kb/decisions/ui', 'kb/decisions/store'] });
    fireEvent.mouseDown(screen.getAllByTestId('picker-drill')[1]);
    await waitFor(() => expect(screen.getByTestId('picker-up')).toBeTruthy());
    expect(screen.getByTestId('picker-up').textContent).toContain('kb/decisions');

    completions.mockResolvedValue({ values: ['kb/architecture', 'kb/decisions'] });
    fireEvent.mouseDown(screen.getByTestId('picker-up'));
    await waitFor(() => expect(screen.queryByTestId('picker-up')).toBeNull());
    expect(completions).toHaveBeenLastCalledWith('core', 'agent/main', 'path', 'kb/');
  });

  it('ignores a category response the reader has already hovered past', async () => {
    // The rail opens on hover, so a sweep down it starts a request per row. A
    // slow `domain` landing after the reader settled on `type` would render
    // domain values under the Types heading — and clicking one there mints
    // `type:<a domain name>`, a chip that matches nothing at all.
    const TYPES = ['observation', 'concept'];
    let releaseDomains: (v: unknown) => void = () => {};
    completions.mockImplementationOnce(() => new Promise(res => { releaseDomains = res; }));
    render(<FilterBar state={state} dispatch={vi.fn()} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    fireEvent.mouseEnter(screen.getByTestId('picker-cat-domain'));

    completions.mockResolvedValue({ values: TYPES });
    fireEvent.mouseEnter(screen.getByTestId('picker-cat-type'));
    await waitFor(() => expect(shown()).toEqual(TYPES));

    // act() so the late resolution is flushed all the way through a re-render —
    // a bare `await` returns before React has committed, which would let this
    // pass against the very bug it exists to catch.
    await act(async () => {
      releaseDomains({ values: DOMAINS });
      await new Promise(r => setTimeout(r, 0));
    });
    expect(shown()).toEqual(TYPES);
  });
});

// jsdom serialises inline `color` as rgb().
function hexToRgb(hex: string): string {
  const h = hex.length === 4
    ? '#' + [...hex.slice(1)].map(c => c + c).join('')
    : hex;
  const [r, g, b] = [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16));
  return `rgb(${r}, ${g}, ${b})`;
}

// The chip is what the picker produces, so the two must agree on colour. They
// disagreed for entity — blue in the summary and on the fact body, pink only
// here — and the blue was unavailable because every type chip was wearing it.
describe('FilterBar — chip colours', () => {
  beforeEach(() => { vi.clearAllMocks(); completions.mockResolvedValue({ values: [] }); });

  const chipFor = (category: string, value: string) => {
    render(<FilterBar state={{ ...state, filters: [{ category, value }] } as AppState}
      dispatch={vi.fn()} />);
    return screen.getByTestId('filter-chip');
  };

  it('gives an entity chip the blue it has everywhere else', () => {
    expect(chipFor('entity', 'Anthropic').style.color).toBe('rgb(136, 170, 255)');
  });

  it('gives each type chip its OWN colour and glyph, not one blue for all twelve', () => {
    const obs = chipFor('type', 'observation');
    expect(obs.style.color).toBe(hexToRgb(typeStyles.observation.color));
    expect(obs.textContent).toContain(typeStyles.observation.icon);
    cleanup();
    // Compared against observation rather than recomputed: typeStyles.policy is
    // '#f9b4', a FOUR-digit hex, so CSS reads it as RGBA at 27% alpha. That is a
    // palette bug (every sibling is 3- or 6-digit) and not this test's business
    // — asserting the literal would freeze the typo into the suite.
    const pol = chipFor('type', 'policy');
    expect(pol.style.color).not.toBe('');
    expect(pol.style.color).not.toBe(hexToRgb(typeStyles.observation.color));
  });

  it('leaves entity and type distinguishable — the reason type had to move', () => {
    const entity = chipFor('entity', 'Anthropic').style.color;
    cleanup();
    expect(chipFor('type', 'observation').style.color).not.toBe(entity);
  });

  it('carries the provenance glyph on an origin chip', () => {
    expect(chipFor('origin', 'distilled').textContent).toContain('⚗');
  });

  it('leaves a chip with no glyph glyphless rather than inventing one', () => {
    render(<FilterBar state={{ ...state, filters: [{ category: 'domain', value: 'ai' }] } as AppState}
      dispatch={vi.fn()} />);
    expect(screen.queryByTestId('chip-glyph')).toBeNull();
  });
});
