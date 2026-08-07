import { describe, it, expect, vi, afterEach } from 'vitest';
import { relativeTime, relativeTimeEpoch, typeStyles, defaultTypeStyle, LENS, repoHue, repoHueBg, repoHueBorder, shortBranch } from './utils';

describe('relativeTime', () => {
  afterEach(() => { vi.useRealTimers(); });

  it('returns "just now" for recent dates', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-03-18T12:00:00Z'));
    expect(relativeTime('2026-03-18T12:00:00Z')).toBe('just now');
  });

  it('returns minutes ago', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-03-18T12:05:00Z'));
    expect(relativeTime('2026-03-18T12:00:00Z')).toBe('5m ago');
  });

  it('returns hours ago', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-03-18T15:00:00Z'));
    expect(relativeTime('2026-03-18T12:00:00Z')).toBe('3h ago');
  });

  it('returns days ago', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-03-25T12:00:00Z'));
    expect(relativeTime('2026-03-18T12:00:00Z')).toBe('7d ago');
  });

  it('returns locale date for > 30 days', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-06-01T12:00:00Z'));
    const result = relativeTime('2026-03-18T12:00:00Z');
    expect(result).not.toContain('ago');
  });
});

describe('relativeTimeEpoch', () => {
  it('returns empty string for 0', () => {
    expect(relativeTimeEpoch(0)).toBe('');
  });

  it('delegates to relativeTime for valid epoch', () => {
    vi.useFakeTimers();
    const epoch = Math.floor(new Date('2026-03-18T12:00:00Z').getTime() / 1000);
    vi.setSystemTime(new Date('2026-03-18T12:05:00Z'));
    expect(relativeTimeEpoch(epoch)).toBe('5m ago');
    vi.useRealTimers();
  });
});

describe('typeStyles', () => {
  it('has entries for all 10 epistemic types', () => {
    const expectedTypes = ['observation', 'concept', 'process', 'principle', 'pattern', 'reference', 'synthesis', 'insight', 'hypothesis', 'methodology'];
    for (const t of expectedTypes) {
      expect(typeStyles[t]).toBeDefined();
      expect(typeStyles[t].color).toBeTruthy();
      expect(typeStyles[t].bg).toBeTruthy();
      expect(typeStyles[t].label).toBe(t);
      expect(typeStyles[t].icon).toBeTruthy();
    }
  });

  it('hypothesis has distinct styling', () => {
    expect(typeStyles.hypothesis.color).toBe('#f8a');
    expect(typeStyles.hypothesis.icon).toBe('?');
  });

  it('methodology has distinct styling', () => {
    expect(typeStyles.methodology.color).toBe('#af8');
    expect(typeStyles.methodology.icon).toBe('⚙');
  });

  it('insight has distinct styling', () => {
    expect(typeStyles.insight.color).toBe('#ffcc33');
    expect(typeStyles.insight.icon).toBe('✸');
  });

  it('defaultTypeStyle has unknown label', () => {
    expect(defaultTypeStyle.label).toBe('unknown');
  });
});

describe('LENS tokens', () => {
  it('carries the verbatim design-handoff hex values', () => {
    expect(LENS.accent).toBe('#a8a4f0');
    expect(LENS.bg).toBe('#1c1a2e');
    expect(LENS.border).toBe('#38345c');
    expect(LENS.soft).toBe('#231f38');
    expect(LENS.text).toBe('#141230');
  });
});

describe('repoHue', () => {
  const sample = ['core', 'docs', 'infra', 'research', 'papers', 'scratch'];

  it('is a #rrggbb hex string', () => {
    for (const name of sample) {
      expect(repoHue(name)).toMatch(/^#[0-9a-f]{6}$/);
    }
  });

  it('is stable across calls (same in = same out)', () => {
    for (const name of sample) {
      expect(repoHue(name)).toBe(repoHue(name));
    }
  });

  it('is pairwise distinct over the sample set', () => {
    const hues = sample.map(repoHue);
    expect(new Set(hues).size).toBe(sample.length);
  });

  it('exposes bg/border helpers as the hue plus 1f/44 alpha suffixes', () => {
    for (const name of sample) {
      const base = repoHue(name);
      expect(repoHueBg(name)).toBe(base + '1f');
      expect(repoHueBorder(name)).toBe(base + '44');
      expect(repoHueBg(name)).toMatch(/^#[0-9a-f]{8}$/);
      expect(repoHueBorder(name)).toMatch(/^#[0-9a-f]{8}$/);
    }
  });
});

// `policy` shipped as '#f9b4' — a FOUR-digit hex, which CSS reads as #RGBA. It
// rendered at 27% alpha everywhere a type is drawn (Library rows, highlight
// markers, the summary's Types column, the filter chip) while every sibling was
// solid, and nothing caught it because a washed-out colour still looks like a
// colour. A 4- or 8-digit literal in this palette is always the bug, never the
// intent: alpha belongs in an rgba() at the point of use.
describe('typeStyles palette', () => {
  it('carries no accidental alpha — every colour is a 3- or 6-digit hex', () => {
    for (const [type, style] of Object.entries(typeStyles)) {
      expect(`${type}:${style.color}`).toMatch(/:#([0-9a-f]{3}|[0-9a-f]{6})$/);
      expect(`${type}:${style.bg}`).toMatch(/:#([0-9a-f]{3}|[0-9a-f]{6})$/);
    }
  });

  it('keeps every pair of types visually apart', () => {
    // Hue alone is the wrong metric — heuristic and synthesis sit 3 degrees
    // apart and are plainly different (amber vs orange) because they differ 23%
    // in lightness. This measures weighted RGB distance instead.
    //
    // The threshold is set just under the palette's own tightest pair
    // (concept/process, 68). It is what stopped policy from simply being
    // un-truncated to '#f9b': that lands 42 from hypothesis — closer than any
    // two types have ever been — trading one invisible type for two
    // indistinguishable ones. It moved to the empty coral slot at 80 instead.
    const rgb = (hex: string) => {
      const h = hex.length === 4 ? '#' + [...hex.slice(1)].map(c => c + c).join('') : hex;
      return [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16));
    };
    const dist = (a: string, b: string) => {
      const [r1, g1, b1] = rgb(a), [r2, g2, b2] = rgb(b);
      const rm = (r1 + r2) / 2;
      return Math.sqrt((2 + rm / 256) * (r1 - r2) ** 2 + 4 * (g1 - g2) ** 2
        + (2 + (255 - rm) / 256) * (b1 - b2) ** 2);
    };
    const types = Object.entries(typeStyles);
    for (let i = 0; i < types.length; i++) {
      for (let j = i + 1; j < types.length; j++) {
        const [a, sa] = types[i], [b, sb] = types[j];
        expect(`${a}/${b} ${Math.round(dist(sa.color, sb.color))}`)
          .toMatch(/ (6[0-9]|[7-9][0-9]|[1-9][0-9]{2,})$/);
      }
    }
  });
});

// The agent branch is built as agent/<sanitized-hostname>-<fp8> (internal/app/
// identity.go). The prefix is constant across every agent branch and the
// fingerprint only disambiguates two agents on ONE host, so the machine name is
// the only part worth 190px of chrome. The full string stays in the title.
describe('shortBranch', () => {
  it('drops the constant prefix and the key fingerprint', () => {
    expect(shortBranch('agent/mindev.local-8ef0cd32')).toBe('mindev.local');
  });

  it('leaves a non-agent branch completely alone', () => {
    expect(shortBranch('main')).toBe('main');
    expect(shortBranch('feat/facet-panel-density')).toBe('feat/facet-panel-density');
  });

  it('keeps hyphens that belong to the hostname', () => {
    // sanitizeHostname turns spaces and other git-illegal chars into hyphens,
    // so the host itself routinely contains them. Only the LAST -<8 hex> goes.
    expect(shortBranch('agent/build-box-01-a1b2c3d4')).toBe('build-box-01');
  });

  it('keeps a trailing segment that is not an 8-hex fingerprint', () => {
    expect(shortBranch('agent/laptop-staging')).toBe('laptop-staging');
    expect(shortBranch('agent/laptop-a1b2c3')).toBe('laptop-a1b2c3');
    expect(shortBranch('agent/laptop-a1b2c3d4e5')).toBe('laptop-a1b2c3d4e5');
  });

  it('never returns empty — a branch that is nothing but a prefix keeps its name', () => {
    // Degenerate, but the top bar must never render a blank where a branch was.
    expect(shortBranch('agent/')).toBe('agent/');
    expect(shortBranch('')).toBe('');
  });
});
