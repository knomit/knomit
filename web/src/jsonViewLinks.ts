import { useEffect, useMemo, useState } from 'react';
import { resolveFactHandles } from './chatApi';
import type { ResolvedFact } from './chatFactLinks';
import { kbRefs } from './jsonViewText';

// The kb:// fact paths of a tool result's JSON view, resolved to the facts they
// name, so the view can draw them as links (JsonView's `links`).

/** The kb:// paths that resolved to a fact, by the path as it is written. */
export type FactLinks = ReadonlyMap<string, ResolvedFact>;

/** The most handles one POST /facts/resolve takes (the server's maxResolveHandles). */
const RESOLVE_BATCH = 100;

/** What each kb:// path resolved to (null: not a fact), for the page's life. */
const resolved = new Map<string, ResolvedFact | null>();

/** For tests: forget every answer. */
export function resetResultLinks(): void { resolved.clear(); }

/**
 * useKbLinks resolves the kb:// paths in a text through POST /facts/resolve,
 * the route the chat's fact links use: the server reads the repo id (or name)
 * in the path and answers with the repo, its branch and the path. A path it
 * does not answer stays plain text, as does one that names a commit. Each
 * path is asked once for the page's life, in batches the server accepts.
 */
export function useKbLinks(text: string | null): FactLinks {
  const refs = useMemo(() => (text === null ? [] : [...new Set(kbRefs(text).map(r => r.ref))]), [text]);
  const [version, setVersion] = useState(0);
  useEffect(() => {
    let cancelled = false;
    const ask = refs.filter(r => !resolved.has(r));
    for (let i = 0; i < ask.length; i += RESOLVE_BATCH) {
      const batch = ask.slice(i, i + RESOLVE_BATCH);
      const settle = (list: ResolvedFact[]) => {
        const by = new Map(list.filter(f => !f.commit).map(f => [f.handle, f]));
        for (const h of batch) resolved.set(h, by.get(h) ?? null);
        if (!cancelled) setVersion(v => v + 1);
      };
      resolveFactHandles(batch).then(settle, () => settle([]));
    }
    return () => { cancelled = true; };
  }, [refs]);
  return useMemo(() => {
    const m = new Map<string, ResolvedFact>();
    for (const r of refs) {
      const f = resolved.get(r);
      if (f) m.set(r, f);
    }
    return m;
    // version: a batch settled, so `resolved` holds more than it did.
  }, [refs, version]); // eslint-disable-line react-hooks/exhaustive-deps
}
