import { api } from './api';
import type { MotifsQuery, MotifsPage, MotifCluster } from './api';
import type { AppState } from './state';
import { isLensContext } from './state';

/**
 * Which REST surface a motif vocabulary read goes to.
 *
 * NOT called a "scope": `path` is already the scope in every motif surface —
 * "which corpus this vocabulary is of" — and reusing the word for the
 * repo/lens axis would put two different questions under one name. This names
 * the only thing that actually differs between the two reads: the endpoint.
 *
 * The two answer with the SAME shape. A lens's vocabulary is every mount's
 * clusters merged into one list, in the repo endpoint's own envelope, with the
 * same query and the same defaults — the server keeps it that way through one
 * shared renderer behind both handlers. So everything above this file works in
 * both contexts without branching, and the branch lives here, once.
 */
export type MotifEndpoint =
  | { kind: 'repo'; repo: string; branch: string }
  | { kind: 'lens'; lens: string };

/** The endpoint the app's current context reads its vocabulary from. */
export function motifEndpointOf(s: AppState): MotifEndpoint {
  if (isLensContext(s) && s.context.kind === 'lens') {
    return { kind: 'lens', lens: s.context.name };
  }
  return { kind: 'repo', repo: s.repo, branch: s.branch };
}

/**
 * Whether the endpoint is ADDRESSABLE yet.
 *
 * App picks a repo from /api/v1/repos on mount, so the first render has no
 * repo, and a request then is a certain 404 that would render as a failure of
 * the motifs themselves rather than of the timing. Waiting is the honest state.
 */
export function motifEndpointReady(e: MotifEndpoint): boolean {
  return e.kind === 'lens' ? !!e.lens : !!e.repo && !!e.branch;
}

/**
 * Whether the VOCABULARY COLLECTION can be read here.
 *
 * Addressable, and the client actually has the method: the vendored /explore
 * build swaps api.ts for a static bundle, and a control that opened onto a
 * permanent error is worse than no control.
 *
 * Kept apart from canReadMotifCluster below because the two are different
 * questions with different answers — a bundle can carry one read and not the
 * other — and collapsing them would make one surface's absence silently
 * govern the other's.
 */
export function canReadMotifs(e: MotifEndpoint): boolean {
  return motifEndpointReady(e) &&
    (e.kind === 'lens' ? typeof api.lensMotifs === 'function' : typeof api.motifs === 'function');
}

/** Whether ONE cluster can be read here. See canReadMotifs. */
export function canReadMotifCluster(e: MotifEndpoint): boolean {
  return motifEndpointReady(e) &&
    (e.kind === 'lens' ? typeof api.lensMotifCluster === 'function' : typeof api.motifCluster === 'function');
}

/** The vocabulary collection, from whichever surface `e` names. */
export function readMotifs(e: MotifEndpoint, opts?: MotifsQuery): Promise<MotifsPage> {
  return e.kind === 'lens'
    ? api.lensMotifs(e.lens, opts)
    : api.motifs(e.repo, e.branch, opts);
}

/** One cluster, by cluster_key or by any member spelling. */
export function readMotifCluster(e: MotifEndpoint, key: string): Promise<MotifCluster> {
  return e.kind === 'lens'
    ? api.lensMotifCluster(e.lens, key)
    : api.motifCluster(e.repo, e.branch, key);
}

/** A stable string identity for an endpoint, for effect dependency arrays. */
export function motifEndpointKey(e: MotifEndpoint): string {
  return e.kind === 'lens' ? `lens\0${e.lens}` : `repo\0${e.repo}\0${e.branch}`;
}
