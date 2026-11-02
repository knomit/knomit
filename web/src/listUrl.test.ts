import { describe, expect, it } from 'vitest';
import { EMPTY_LIST, contextOf, parseListUrl, writeListUrl } from './listUrl';
import type { ListUrlState, UrlList } from './listUrl';
import { parseFactSetParams } from './factsetOptions';
import { parseMaterializeParams } from './materializeOptions';

// The list's URL keys: each one round-trips on its own, the context is named by
// stable uid (repo and lens alike), only non-defaults are written, and nothing
// collides with materialize / mat_* / factset / fs_*.
//
// Sabotage, one run each: drop the `l_rp` (pivot return path) write; write
// lensSources [] as no key (it reads back as null, every mount); write the
// list without a context.

const repo = (list: Partial<UrlList>): ListUrlState => ({ ctx: { kind: 'repo', uid: 'u-a', branch: 'exp/try' }, list: { ...EMPTY_LIST, ...list } });
const lens = (list: Partial<UrlList>): ListUrlState => ({ ctx: { kind: 'lens', uid: 'L1' }, list: { ...EMPTY_LIST, ...list } });
const roundTrip = (s: ListUrlState) => parseListUrl(writeListUrl('', s));

describe('list URL keys round-trip one by one', () => {
  const cases: [string, ListUrlState, string][] = [
    ['free text', repo({ freeText: 'go routine "x"' }), 'l_q'],
    ['a content chip', repo({ filters: [{ category: 'type', value: 'policy' }] }), 'l_f'],
    ['chips with colons, every category', repo({ filters: [
      { category: 'domain', value: 'ai:ml' }, { category: 'entity', value: 'Composer 2' }, { category: 'type', value: 'policy' },
      { category: 'kind', value: 'pragmatic' }, { category: 'origin', value: 'authored' }, { category: 'ep', value: 'x' },
      { category: 'path', value: 'kb/gotchas' }, { category: 'motif', value: 'identifier-collision' },
    ] }), 'l_f'],
    ['the pivot return path', repo({ filters: [{ category: 'motif', value: 'm', returnPath: 'kb/a/b' }] }), 'l_rp'],
    ['sort recent', repo({ sort: 'recent' }), 'l_sort'],
    ['sort relevance', repo({ sort: 'relevance' }), 'l_sort'],
    ['since_fork', repo({ sinceFork: true }), 'l_since_fork'],
    ['lens sources', lens({ lensSources: ['docs', 'core'] }), 'l_src'],
    ['lens sources: none selected', lens({ lensSources: [] }), 'l_src'],
  ];
  for (const [name, s, key] of cases) {
    it(name, () => {
      const q = writeListUrl('', s);
      expect(new URLSearchParams(q).has(key)).toBe(true);
      expect(roundTrip(s)).toEqual(s);
    });
  }

  it('the defaults write no list key at all', () => {
    expect(writeListUrl('', repo({}))).toBe('?ctx_repo=u-a&ctx_branch=exp%2Ftry');
    expect(writeListUrl('', lens({}))).toBe('?ctx_lens=L1');
    expect(roundTrip(lens({})).list.lensSources).toBeNull();
  });
});

describe('the context keys', () => {
  it('repo: uid and branch round-trip; lens: uid only', () => {
    expect(roundTrip(repo({})).ctx).toEqual({ kind: 'repo', uid: 'u-a', branch: 'exp/try' });
    expect(roundTrip(lens({})).ctx).toEqual({ kind: 'lens', uid: 'L1' });
    // Both, or neither, is no context.
    expect(parseListUrl('?ctx_repo=a&ctx_lens=b').ctx).toBeNull();
    expect(parseListUrl('?l_q=x').ctx).toBeNull();
  });

  it('contextOf names the repo by its uid, never its name; a lens by its own uid', () => {
    const uid = (n: string) => ({ alpha: 'u-a' } as Record<string, string>)[n];
    expect(contextOf({ kind: 'repo', repo: 'alpha' }, 'machine/x', uid, undefined)).toEqual({ kind: 'repo', uid: 'u-a', branch: 'machine/x' });
    expect(contextOf({ kind: 'repo', repo: 'alpha' }, '', uid, undefined)).toBeNull(); // branch not known yet
    expect(contextOf({ kind: 'repo', repo: 'gone' }, 'b', uid, undefined)).toBeNull();
    expect(contextOf({ kind: 'lens', name: 'eng' }, 'b', uid, 'L1')).toEqual({ kind: 'lens', uid: 'L1' });
    expect(contextOf({ kind: 'lens', name: 'eng' }, 'b', uid, undefined)).toBeNull();
  });

  it('a list without its context writes none of our keys', () => {
    expect(writeListUrl('?l_q=old&ctx_repo=u&x=1', { ctx: null, list: { ...EMPTY_LIST, freeText: 'x' } })).toBe('?x=1');
  });
});

describe('no collision with the materialize and FactSet keys', () => {
  it('keeps materialize/mat_*/factset/fs_* and reads none of them', () => {
    const base = '?materialize=1&mat_depth=6&factset=1&fs_depth=7';
    const q = writeListUrl(base, repo({ freeText: 'x', sort: 'recent' }));
    const p = new URLSearchParams(q);
    expect(p.get('materialize')).toBe('1');
    expect(p.get('mat_depth')).toBe('6');
    expect(p.get('factset')).toBe('1');
    expect(p.get('fs_depth')).toBe('7');
    expect(parseFactSetParams(q).options.depth).toBe(7);
    expect(parseMaterializeParams(q).options.depth).toBe(6);
    expect(parseListUrl(base).list).toEqual(EMPTY_LIST);
  });
});
