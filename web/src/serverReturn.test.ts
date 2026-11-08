import { describe, it, expect, afterEach } from 'vitest';
import { restoreReturnPoint, takeReturnPoint } from './serverReturn';

afterEach(() => { document.body.innerHTML = ''; });

describe('takeReturnPoint', () => {
  it('records scroll only inside the shown containers that are about to be hidden', () => {
    document.body.innerHTML = `
      <div id="outside"></div>
      <div data-under-server="" style="display: contents"><div id="shown"></div></div>
      <div data-under-server="" style="display: none"><div id="hidden"></div></div>`;
    const el = (id: string) => document.getElementById(id)!;
    el('outside').scrollTop = 10; el('shown').scrollTop = 20; el('hidden').scrollTop = 30;
    const p = takeReturnPoint();
    expect(p.scrolled.map(([e]) => e.id)).toEqual(['shown']);
    el('shown').scrollTop = 0;
    restoreReturnPoint(p);
    expect(el('shown').scrollTop).toBe(20);
  });
});
