import { describe, it, expect, vi, afterEach } from 'vitest';
import { relativeTime, relativeTimeEpoch, opStyles, defaultOpStyle, typeStyles, defaultTypeStyle, LENS, repoHue, repoHueBg, repoHueBorder } from './utils';

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

describe('opStyles', () => {
  it('has entries for known operations', () => {
    expect(opStyles.learn).toBeDefined();
    expect(opStyles.update).toBeDefined();
    expect(opStyles.retract).toBeDefined();
    expect(opStyles.subsume).toBeDefined();
    expect(opStyles.sync).toBeDefined();
    expect(opStyles.other).toBeDefined();
  });

  it('defaultOpStyle has empty label', () => {
    expect(defaultOpStyle.label).toBe('');
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
