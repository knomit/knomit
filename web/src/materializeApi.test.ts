import { afterEach, describe, expect, it, vi } from 'vitest';
import { DEFAULT_OPTIONS, PAGE_LIMIT, materializeApi } from './materializeApi';
import { saveTextFile } from './saveFile';

const T = { repo: 'kb', branch: 'main', path: 'kb/decisions/x/abc12345.md' };
const page = { facts: [], cursor: null, has_more: false };

afterEach(() => vi.unstubAllGlobals());

function stubFetch(res: () => Response) {
  const f = vi.fn(async (url: string) => (url ? res() : res()));
  vi.stubGlobal('fetch', f);
  return f;
}
const urlOf = (f: ReturnType<typeof stubFetch>, i: number) => new URL(f.mock.calls[i][0], 'http://x');

describe('pages calls', () => {
  it('a FIRST call sends limit=5 with the options, including after a restart or a count raise', async () => {
    const f = stubFetch(() => new Response(JSON.stringify(page)));
    await materializeApi.page(T, DEFAULT_OPTIONS, null);
    await materializeApi.page(T, { ...DEFAULT_OPTIONS, depth: 7, unpin: true }, null); // a restart
    await materializeApi.page(T, { ...DEFAULT_OPTIONS, references: 399 }, null); // a count raise
    expect(PAGE_LIMIT).toBe(5);
    for (let i = 0; i < 3; i++) {
      const u = urlOf(f, i);
      expect(u.pathname.endsWith('/materialize/pages')).toBe(true);
      expect(u.searchParams.get('limit')).toBe('5');
      expect(u.searchParams.has('cursor')).toBe(false);
      expect(u.searchParams.has('depth')).toBe(true);
    }
    expect(urlOf(f, 1).searchParams.get('depth')).toBe('7');
    expect(urlOf(f, 2).searchParams.get('max_nodes')).toBe('400');   // references + 1: the root counts on the server
    expect(urlOf(f, 0).searchParams.get('max_nodes')).toBe('200');   // the default request is unchanged (199 references)
  });

  it('sends max_nodes = references + 1 at both ends: 0 -> 1, 499 -> 500', async () => {
    const f = stubFetch(() => new Response(JSON.stringify(page)));
    await materializeApi.page(T, { ...DEFAULT_OPTIONS, references: 0 }, null);
    await materializeApi.page(T, { ...DEFAULT_OPTIONS, references: 499 }, null);
    expect(urlOf(f, 0).searchParams.get('max_nodes')).toBe('1');
    expect(urlOf(f, 1).searchParams.get('max_nodes')).toBe('500');
  });

  it('a CURSOR call sends only the cursor: no limit, no options (the server refuses them with a 400)', async () => {
    const f = stubFetch(() => new Response(JSON.stringify(page)));
    await materializeApi.page(T, { ...DEFAULT_OPTIONS, depth: 7 }, 'cur-1');
    expect([...urlOf(f, 0).searchParams.keys()]).toEqual(['cursor']);
    expect(urlOf(f, 0).searchParams.get('cursor')).toBe('cur-1');
  });
});

describe('export of a compressed body', () => {
  it('does not mistake the wire Content-Length for the decoded size, and saves the whole text', async () => {
    const text = '# Doc\n\n' + 'word '.repeat(200);
    // fetch hands the DECODED body; Content-Length is the (smaller) gzip size
    stubFetch(() => new Response(text, { headers: { 'Content-Encoding': 'gzip', 'Content-Length': '57', 'Content-Disposition': 'attachment; filename="x.md"' } }));
    const doc = await materializeApi.exportMarkdown(T, DEFAULT_OPTIONS);
    expect(doc.text).toBe(text);
    let saved = '';
    const out = await saveTextFile('x.md', doc.text, 'text/markdown', {
      desktop: false, nativeWrite: async () => '', download: (_n, t) => { saved = t; },
    });
    expect(out.kind).toBe('downloaded');
    expect(saved).toBe(text);
  });

  it('still refuses an UNcompressed body that ended early', async () => {
    stubFetch(() => new Response('short', { headers: { 'Content-Length': '500' } }));
    await expect(materializeApi.exportMarkdown(T, DEFAULT_OPTIONS)).rejects.toThrow(/ended early/);
  });
});
