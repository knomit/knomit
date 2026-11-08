import { describe, it, expect, vi } from 'vitest';
import type { KeyboardEvent as ReactKeyboardEvent } from 'react';
import { ago, left, listKeys, relDays, tabStop } from './accessUtil';

const now = Date.parse('2026-09-24T12:00:00Z');
const at = (ms: number) => new Date(now + ms).toISOString();
const MIN = 60_000;
const DAY = 86_400_000;

describe('date wording', () => {
  it('relDays names the day relative to now', () => {
    expect(relDays(at(0), now)).toBe('today');
    expect(relDays(at(DAY), now)).toBe('tomorrow');
    expect(relDays(at(-DAY), now)).toBe('yesterday');
    expect(relDays(at(10 * DAY), now)).toBe('in 10 days');
    expect(relDays(at(-3 * DAY), now)).toBe('3 days ago');
  });

  it('ago says minutes, then hours, then days', () => {
    expect(ago(at(-20_000), now)).toBe('just now');
    expect(ago(at(-40 * MIN), now)).toBe('40 min ago');
    expect(ago(at(-60 * MIN), now)).toBe('1 hour ago');
    expect(ago(at(-6 * 60 * MIN), now)).toBe('6 hours ago');
    expect(ago(at(-12 * DAY), now)).toBe('12 days ago');
  });

  it('left counts a short deadline down, rounding minutes up, and says expired at zero', () => {
    expect(left(at(8 * MIN), now)).toBe('8 min left');
    expect(left(at(7 * MIN + 1_000), now)).toBe('8 min left');
    expect(left(at(40_000), now)).toBe('40 s left');
    expect(left(at(0), now)).toBe('expired');
    expect(left(at(-MIN), now)).toBe('expired');
  });
});

// listKeys reads only the event's key, modifiers, target and currentTarget.
function key(k: string, over: { target?: HTMLElement; ctrlKey?: boolean } = {}) {
  const list = document.createElement('div');
  const e = {
    key: k, metaKey: false, ctrlKey: over.ctrlKey ?? false, altKey: false,
    target: over.target ?? list, currentTarget: list, preventDefault: vi.fn(),
  };
  return e as unknown as ReactKeyboardEvent<HTMLElement> & { preventDefault: ReturnType<typeof vi.fn> };
}

describe('listKeys', () => {
  it('steps the selection with ↑/↓ and j/k, clamped at both ends, starting from the top', () => {
    const select = vi.fn();
    listKeys(['a', 'b', 'c'], null, select, {})(key('ArrowDown'));
    expect(select).toHaveBeenLastCalledWith('a');
    listKeys(['a', 'b', 'c'], 'a', select, {})(key('j'));
    expect(select).toHaveBeenLastCalledWith('b');
    listKeys(['a', 'b', 'c'], 'c', select, {})(key('ArrowDown'));
    expect(select).toHaveBeenLastCalledWith('c');
    listKeys(['a', 'b', 'c'], 'a', select, {})(key('k'));
    expect(select).toHaveBeenLastCalledWith('a');
  });

  it('leaves to the rail with ←, and on to the detail with → or Enter only with something picked', () => {
    const onLeft = vi.fn();
    const onRight = vi.fn();
    listKeys(['a'], null, vi.fn(), { onLeft, onRight })(key('ArrowLeft'));
    expect(onLeft).toHaveBeenCalledOnce();
    listKeys(['a'], null, vi.fn(), { onLeft, onRight })(key('ArrowRight'));
    expect(onRight).not.toHaveBeenCalled();
    listKeys(['a'], 'a', vi.fn(), { onLeft, onRight })(key('Enter'));
    expect(onRight).toHaveBeenCalledOnce();
  });

  it('never takes a key typed into a field, or one with a modifier', () => {
    const select = vi.fn();
    listKeys(['a', 'b'], 'a', select, {})(key('j', { target: document.createElement('input') }));
    listKeys(['a', 'b'], 'a', select, {})(key('ArrowDown', { ctrlKey: true }));
    expect(select).not.toHaveBeenCalled();
  });
});

describe('listKeys at one column (focusOnly)', () => {
  function list(ids: string[]) {
    const el = document.createElement('div');
    for (const id of ids) {
      const b = document.createElement('button');
      b.setAttribute('data-item', id);
      el.appendChild(b);
    }
    document.body.appendChild(el);
    return el;
  }
  const row = (el: HTMLElement, id: string) => el.querySelector<HTMLElement>(`[data-item="${id}"]`)!;
  function press(el: HTMLElement, k: string, target: HTMLElement) {
    const e = { key: k, metaKey: false, ctrlKey: false, altKey: false, target, currentTarget: el, preventDefault: vi.fn() };
    return e as unknown as ReactKeyboardEvent<HTMLElement> & { preventDefault: ReturnType<typeof vi.fn> };
  }

  it('moves the focus from the focused row and never selects', () => {
    const el = list(['a', 'b', 'c']);
    const select = vi.fn();
    listKeys(['a', 'b', 'c'], null, select, { focusOnly: true })(press(el, 'ArrowDown', el));
    expect(document.activeElement).toBe(row(el, 'a'));
    listKeys(['a', 'b', 'c'], null, select, { focusOnly: true })(press(el, 'j', row(el, 'a')));
    expect(document.activeElement).toBe(row(el, 'b'));
    listKeys(['a', 'b', 'c'], null, select, { focusOnly: true })(press(el, 'k', row(el, 'b')));
    expect(document.activeElement).toBe(row(el, 'a'));
    expect(select).not.toHaveBeenCalled();
    el.remove();
  });

  it('leaves Enter to the focused row\'s own click', () => {
    const el = list(['a']);
    const onRight = vi.fn();
    const e = press(el, 'Enter', row(el, 'a'));
    listKeys(['a'], 'a', vi.fn(), { focusOnly: true, onRight })(e);
    expect(onRight).not.toHaveBeenCalled();
    expect(e.preventDefault).not.toHaveBeenCalled();
    el.remove();
  });
});

describe('tabStop', () => {
  it('is the selected row when it is listed, else the first, else none', () => {
    expect(tabStop(['a', 'b'], 'b')).toBe('b');
    expect(tabStop(['a', 'b'], null)).toBe('a');
    expect(tabStop(['a', 'b'], 'gone')).toBe('a');
    expect(tabStop([], null)).toBeNull();
  });
});
