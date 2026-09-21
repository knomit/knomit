import { describe, it, expect, vi, afterEach } from 'vitest';
import { relAge, daysUntil, expiryShort, expiryLong, changedSinceFork } from './experimentText';

// Every case below pins WHICH value, not merely that a string came back: the
// whole risk in this module is a plausible-looking wrong number (a stale
// "28 days" after expiry moved, or "0 days" where the answer is "never").

afterEach(() => { vi.useRealTimers(); });

/** Freezes the clock so an age is a fixed arithmetic result, not a race. */
function at(iso: string) {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(iso));
}

describe('relAge', () => {
  it('counts minutes, then hours, then days', () => {
    at('2026-09-20T12:00:00Z');
    expect(relAge('2026-09-20T11:48:00Z')).toBe('12m ago');
    expect(relAge('2026-09-20T09:00:00Z')).toBe('3h ago');
    expect(relAge('2026-09-14T12:00:00Z')).toBe('6d ago');
  });

  it('renders a placeholder for absent and unparseable values, never a date', () => {
    expect(relAge(undefined)).toBe('—');
    expect(relAge('')).toBe('—');
    expect(relAge('not a date')).toBe('—');
  });

  it('clamps a future timestamp to 0 rather than reporting a negative age', () => {
    at('2026-09-20T12:00:00Z');
    expect(relAge('2026-09-20T12:30:00Z')).toBe('0m ago');
  });
});

describe('daysUntil', () => {
  it('distinguishes NEVER (null) from EXPIRES NOW (0 or less)', () => {
    at('2026-09-20T12:00:00Z');
    // Absent expiry means the sweeper is off. Collapsing this to 0 would make
    // the UI say an experiment is expiring when nothing will ever remove it.
    expect(daysUntil(undefined)).toBeNull();
    expect(daysUntil('2026-09-19T12:00:00Z')).toBeLessThanOrEqual(0);
    expect(daysUntil('2026-10-18T12:00:00Z')).toBe(28);
  });
});

describe('expiryShort', () => {
  it('names the gap in days, and — when nothing expires', () => {
    at('2026-09-20T12:00:00Z');
    expect(expiryShort('2026-10-18T12:00:00Z')).toBe('in 28 days');
    expect(expiryShort('2026-09-21T12:00:00Z')).toBe('in 1 day');
    expect(expiryShort('2026-09-19T12:00:00Z')).toBe('now');
    expect(expiryShort(undefined)).toBe('—');
  });
});

describe('expiryLong', () => {
  it('names the CONDITION, not just the date', () => {
    at('2026-09-20T12:00:00Z');
    expect(expiryLong('2026-10-18T12:00:00Z')).toBe('expires in 28 days without a commit');
    expect(expiryLong('2026-09-21T12:00:00Z')).toBe('expires in 1 day without a commit');
  });

  it('is EMPTY when nothing expires, so the band drops the segment', () => {
    // Not the string "never": the band tests this for falsiness to decide
    // whether to render a separator, and a non-empty "never" would print a
    // policy line on every screen for a server that has expiry switched off.
    expect(expiryLong(undefined)).toBe('');
  });
});

describe('changedSinceFork', () => {
  it('singularises one fact', () => {
    expect(changedSinceFork(1)).toBe('1 fact changed since the fork');
    expect(changedSinceFork(0)).toBe('0 facts changed since the fork');
    expect(changedSinceFork(11)).toBe('11 facts changed since the fork');
  });
});
