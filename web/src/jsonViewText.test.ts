import { describe, it, expect } from 'vitest';
import { dateAt, foldLabel, initialFolds, jsonBlocks, jsonText, kbRefs, resultText, unpackJson } from './jsonViewText';

describe('resultText: what the JSON view shows for a raw result', () => {
  it('pretty-prints JSON with 2-space indents', () => {
    expect(resultText('{"a":1,"b":[true,null]}')).toEqual({ json: true, text: '{\n  "a": 1,\n  "b": [\n    true,\n    null\n  ]\n}' });
  });

  it('falls back to the result exactly as it came when it is not JSON', () => {
    expect(resultText('no lens named "demo-kb"')).toEqual({ json: false, text: 'no lens named "demo-kb"' });
    expect(resultText('{"half": ')).toEqual({ json: false, text: '{"half": ' });
  });

  it('unpacks a string that is itself JSON, recursively', () => {
    const inner = JSON.stringify({ decisions: [{ path: 'p', action: 'keep' }], deeper: JSON.stringify([1, 2]) });
    const { text, json } = resultText(JSON.stringify({ response: inner, note: 'plain' }));
    expect(json).toBe(true);
    expect(JSON.parse(text)).toEqual({ response: { decisions: [{ path: 'p', action: 'keep' }], deeper: [1, 2] }, note: 'plain' });
    expect(text).not.toContain('\\"');
  });

  it('leaves strings that only look like JSON, and scalars, alone', () => {
    expect(unpackJson('{not json}')).toBe('{not json}');
    expect(unpackJson('"quoted"')).toBe('"quoted"');
    expect(unpackJson('42')).toBe('42');
    expect(unpackJson(' [1] ')).toEqual([1]);
  });

  it('keeps a "__proto__" key (it is data, not a prototype), unpacked or not', () => {
    const { text, json } = resultText('{"__proto__":1,"a":2}');
    expect(json).toBe(true);
    expect(text).toBe('{\n  "__proto__": 1,\n  "a": 2\n}');
    expect(resultText(JSON.stringify({ s: '{"__proto__":{"x":1}}' })).text).toContain('"__proto__": {');
  });

  it('a structure too deep to walk falls back to plain text instead of throwing', () => {
    const raw = '['.repeat(20000) + ']'.repeat(20000);
    expect(() => JSON.parse(raw)).not.toThrow();
    const out = resultText(raw);
    expect(out).toEqual({ text: raw, json: false });
    // The same depth arriving as a call's arguments does not throw either.
    let deep: unknown = [];
    for (let i = 0; i < 20000; i++) deep = [deep];
    expect(() => jsonText(deep)).not.toThrow();
    expect(jsonText(deep).json).toBe(false);
  });

  it('jsonText: a call\'s arguments, unpacked and pretty-printed; no arguments is null', () => {
    expect(jsonText({ text: 'first' })).toEqual({ json: true, text: '{\n  "text": "first"\n}' });
    expect(jsonText(undefined)).toEqual({ json: true, text: 'null' });
  });
});

describe('folding: blocks deeper than two levels start folded', () => {
  const doc = JSON.stringify({ a: { b: { c: { d: { e: 1 } }, c2: [] } }, z: [1] }, null, 2);

  it('jsonBlocks lists the multi-line blocks with their depth; an empty one is not a block', () => {
    const blocks = jsonBlocks(doc);
    expect(blocks.map(b => b.depth)).toEqual([0, 1, 2, 3, 4, 1]);
    for (const b of blocks) expect('{['.includes(doc[b.from - 1]) && '}]'.includes(doc[b.to])).toBe(true);
  });

  it('initialFolds folds depth 3 and below, outermost only', () => {
    const folds = initialFolds(doc);
    expect(folds).toHaveLength(1);
    const [f] = folds;
    expect(JSON.parse(doc.slice(f.from - 1, f.to + 1))).toEqual({ d: { e: 1 } });
  });

  it('a bracket inside a string is not a block', () => {
    const t = JSON.stringify({ a: { b: { c: { s: '{[\n' } } } }, null, 2);
    expect(initialFolds(t)).toHaveLength(1);
    expect(jsonBlocks(t)).toHaveLength(4);
  });

  it('foldLabel counts keys and items at the block\'s own level, singular for one', () => {
    const t = JSON.stringify({ o: { a: 1, b: { x: 1, y: 2 }, c: 's,t' }, one: { k: 1 }, arr: [1, [2, 3], 4], solo: ['x'] }, null, 2);
    const label = (key: string) => {
      const lead = `"${key}": `;
      const b = jsonBlocks(t).find(x => t.slice(x.from - 1 - lead.length, x.from - 1) === lead);
      return b ? foldLabel(t, b.from, b.to) : null;
    };
    expect(label('o')).toBe('3 keys');
    expect(label('one')).toBe('1 key');
    expect(label('arr')).toBe('3 items');
    expect(label('solo')).toBe('1 item');
    expect(foldLabel('x', 1, 1)).toBeNull();
  });
});

describe('kbRefs and dateAt', () => {
  it('finds kb:// fact paths, without a sentence\'s trailing dot', () => {
    const t = '"file": "kb://3ec012f5b4d2/kb/a/b/ca7a68fa.md", "body": "see kb://knomit-kb/kb/x/y.md."';
    expect(kbRefs(t).map(r => r.ref)).toEqual(['kb://3ec012f5b4d2/kb/a/b/ca7a68fa.md', 'kb://knomit-kb/kb/x/y.md']);
    const [r] = kbRefs(t);
    expect(t.slice(r.from, r.to)).toBe(r.ref);
    expect(kbRefs('"src://7b4887ce51d9/web/a.ts@abc"')).toEqual([]);
  });

  it('a number under a *_at key is a date: seconds, or milliseconds at 13 digits; only on the number', () => {
    const line = '      "committed_at": 1794274206,';
    const col = line.indexOf('1794');
    expect(dateAt(line, col)?.date.getTime()).toBe(1794274206000);
    expect(dateAt(line, col + 10)?.date.getTime()).toBe(1794274206000);
    expect(dateAt(line, 3)).toBeNull();
    expect(dateAt('  "at_ms": 1794274206000', 12)).toBeNull();
    expect(dateAt('  "seen_at": 1794274206000', 14)?.date.getTime()).toBe(1794274206000);
    expect(dateAt('  "count": 1794274206', 12)).toBeNull();
    expect(dateAt('  "made_at": "2026-11-09"', 14)).toBeNull();
  });
});
