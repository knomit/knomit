import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api, type ProbeResult } from './api';
import { originURL, initialWizardState } from './wizardState';

// A URL pasted with surrounding whitespace must be SENT as the URL the wizard
// classified, not as the raw field.
//
// The wizard already trimmed for every decision it made — transportFor,
// hostOf, repoNameFromURL and the probe key all call .trim() — and then sent
// state.url raw. A URL pasted with trailing spaces was therefore classified as
// one address and requested as another: the spaces survive url.Parse on the
// server, arrive percent-encoded in the path, and came back to the user as
// "repository not found: 404 page not found" against a URL they could see was
// correct. The server trims too and is the durable fix; this is the client no
// longer asking the wrong question.

vi.mock('./api', () => ({
  api: {
    probeOrigin: vi.fn(),
    probeInitialized: vi.fn(),
    createRepo: vi.fn(),
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
    ]),
    ontologyPresetYAML: vi.fn(async () => 'id: general\nname: General\ntopics:\n  people:\n'),
    validateOntology: vi.fn(async () => ({ ok: true, id: 'x', name: 'X', topics: ['a'], rule_count: 1 })),
    ontologySchema: vi.fn(async () => []),
  },
}));

const probeOrigin = () => api.probeOrigin as ReturnType<typeof vi.fn>;

const populated = (): ProbeResult => ({
  reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'],
});

const CLEAN = 'https://host/git/arxiv-kb';

describe('originURL', () => {
  // Each form separately: the four behave differently on the server before the
  // trim, and a single mixed string could pass on the strength of whichever
  // one happens to work.
  it.each([
    ['trailing space', `${CLEAN} `],
    ['leading space', ` ${CLEAN}`],
    ['tab', `\t${CLEAN}\t`],
    ['newline', `${CLEAN}\n`],
    ['all of them', ` \t\n${CLEAN} \t\n`],
  ])('strips %s', (_name, padded) => {
    expect(originURL({ ...initialWizardState, url: padded })).toBe(CLEAN);
  });

  it('leaves a clean URL alone', () => {
    expect(originURL({ ...initialWizardState, url: CLEAN })).toBe(CLEAN);
  });
});

describe('the wizard sends the URL it classified', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    probeOrigin().mockReset();
  });

  it.each([
    ['trailing space', `${CLEAN}  `],
    ['leading space', `  ${CLEAN}`],
    ['tab', `\t${CLEAN}\t`],
    ['newline', `${CLEAN}\n`],
  ])('probes the trimmed URL when the field carries a %s', async (_name, padded) => {
    probeOrigin().mockResolvedValueOnce(populated());
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-url'), { target: { value: padded } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(probeOrigin()).toHaveBeenCalled());
    // The assertion is on the URL that went OUT, not on the wizard's state:
    // the field may keep what the user typed, but the request may not.
    expect(probeOrigin().mock.calls[0][0]).toMatchObject({ url: CLEAN });
  });
});
