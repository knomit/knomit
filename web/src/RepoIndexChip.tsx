import type { CSSProperties } from 'react';
import type { RepoInfo } from './api';

// RepoIndexChip marks a repository whose background search index is not ready.
//
// The repo page already showed this for the ONE open repo. Everywhere else — the
// rail, the top-bar switcher, the Manage table — a repo mid-heal looked exactly
// like a finished one, so a search that returned less than it should have read
// as missing knowledge rather than as an index still being built. After a
// subscribe of any size, that heal is the longest thing still happening.
//
// It renders NOTHING when the index is ready, and nothing when the server said
// nothing (an older build, or a row with no live store). Absence is not
// "ready": it is "no answer", and both render the same way here because there
// is nothing honest to say in either case. A chip on every ready row would be
// the screen answering a question nobody asked — the same rule RepoStateChip
// follows.
export function RepoIndexChip({ repo }: { repo: Pick<RepoInfo, 'index_state' | 'index_done' | 'index_total'> }) {
  const state = repo.index_state;
  if (state !== 'indexing' && state !== 'error') return null;

  if (state === 'error') {
    return (
      <span
        data-testid="repo-index-error"
        title="The background index did not finish. The repository is there; search over it may be incomplete until it is rebuilt."
        style={{ ...chip, ...errorChip }}
      >index error</span>
    );
  }

  // The counts are the heal's OWN, and absent until it has counted its work.
  // "0/0" would read as a claim about the repo rather than as a count not yet
  // taken, so an uncounted heal says "indexing" and no more.
  const done = repo.index_done ?? 0;
  const total = repo.index_total ?? 0;
  const counts = total > 0 ? ` ${done}/${total}` : '';
  return (
    <span
      data-testid="repo-index-indexing"
      title="The background search index is still being built. Reads work, but may be incomplete until it finishes."
      style={chip}
    >{`indexing${counts}`}</span>
  );
}

const chip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};

// Amber for the error, matching RepoStateChip: the repository and everything
// in it are intact, and what failed is a cache that can be rebuilt. Red is for
// data that is gone.
const errorChip: CSSProperties = {
  color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22',
};
