import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import postcss from 'postcss';

// Nothing else parses the stylesheets, so a comment that lost its opening (its
// tail then glues onto the next selector and a browser drops that whole rule)
// went unnoticed. These tests parse them the way a browser would.
// SABOTAGE: delete the "/* A quote in an answer must not read..." opening in App.css -> red.
const read = (name: string) => readFileSync(resolve(process.cwd(), 'src', name), 'utf8');

describe.each(['App.css', 'markdown.css'])('%s', name => {
  it('has no rule whose selector swallowed comment text', () => {
    const bad: string[] = [];
    postcss.parse(read(name)).walkRules(r => {
      // A real selector holds no comment marker, no newline-separated prose and no declaration colon-space runs.
      if (r.selector.includes('*/') || r.selector.includes('/*') || /^[^.#:[a-zA-Z*&>+~-]/.test(r.selector) || /\*\//.test(r.raws.before ?? '')) bad.push(r.selector);
      if (/(^|\n)\s*: /.test(r.selector) || /\s(rule|ground|italic)\.?\s*\*\//.test(r.selector)) bad.push(r.selector);
    });
    expect(bad).toEqual([]);
  });

  it('has as many comment closes as comment opens', () => {
    const css = read(name);
    expect((css.match(/\*\//g) ?? []).length).toBe((css.match(/\/\*/g) ?? []).length);
  });
});

describe('App.css chat answer quote', () => {
  it('has the .k-prose.k-chat-answer blockquote rule with the quiet quote declarations', () => {
    const decl = new Map<string, string>();
    let found = 0;
    postcss.parse(read('App.css')).walkRules(r => {
      if (r.selector !== '.k-prose.k-chat-answer blockquote') return;
      found++;
      r.walkDecls(d => { decl.set(d.prop, d.value); });
    });
    expect(found).toBeGreaterThan(0);
    expect(decl.get('font-style')).toBe('italic');
    expect(decl.get('color')).toBe('#9a9a9a');
    expect(decl.get('border-left')).toBe('1px solid #2c2c2c');
    expect(decl.get('padding')).toBe('0 0 0 12px');
    expect(decl.get('background')).toBe('none');
  });
});
