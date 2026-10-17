/// <reference types="node" />
import { readFileSync } from 'node:fs';

// process.cwd() is web/ under `npm test`; a bundler-processed ?raw import of a
// stylesheet comes back empty under vitest.
const css = readFileSync('src/App.css', 'utf8');

// jsdom does no layout and evaluates no @media, but it parses the real
// stylesheet into rules, so the CASCADE is decided here on the real App.css:
// for one element at one viewport width, which declaration of a property wins
// (!important, then an inline style, then specificity, then source order), the
// way a browser decides it. A repeated base rule after the @media block, or a
// dropped `!important`/declaration in it, changes the winner.
function mediaApplies(text: string, width: number): boolean {
  const m = /^\(\s*(max|min)-width:\s*(\d+)px\s*\)$/.exec(text.trim());
  if (!m) throw new Error(`connectNarrow.test: unsupported media query ${text}`);
  return m[1] === 'max' ? width <= Number(m[2]) : width >= Number(m[2]);
}
export function computed(el: Element, prop: string, width: number): string {
  const style = document.createElement('style');
  style.textContent = css;
  document.head.appendChild(style);
  try {
    type Cand = { important: boolean; inline: boolean; spec: number; order: number; value: string };
    const cands: Cand[] = [];
    let order = 0;
    const visit = (rules: CSSRuleList) => {
      for (const rule of Array.from(rules)) {
        if ('media' in rule && 'cssRules' in rule) {
          if (mediaApplies((rule as CSSMediaRule).media.mediaText, width)) visit((rule as CSSMediaRule).cssRules);
        } else if ('selectorText' in rule) {
          const r = rule as CSSStyleRule;
          const value = r.style.getPropertyValue(prop);
          if (value && el.matches(r.selectorText)) {
            cands.push({ important: r.style.getPropertyPriority(prop) === 'important', inline: false, spec: (r.selectorText.match(/\./g) ?? []).length, order: order++, value });
          }
        }
      }
    };
    visit(style.sheet!.cssRules);
    const inline = (el as HTMLElement).style.getPropertyValue(prop);
    if (inline) cands.push({ important: false, inline: true, spec: 0, order: order++, value: inline });
    cands.sort((a, b) => Number(b.important) - Number(a.important) || Number(b.inline) - Number(a.inline) || b.spec - a.spec || b.order - a.order);
    return cands[0]?.value ?? '';
  } finally {
    style.remove();
  }
}

