import { describe, it, expect, vi, afterEach } from 'vitest';
import { relativeTime, relativeTimeEpoch, opStyles, defaultOpStyle } from './utils';

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
