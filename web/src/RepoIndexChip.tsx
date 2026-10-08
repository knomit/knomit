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
//
// COMPACT is for a row whose width is fixed and whose NAME is the point — the
// manage rail. There "indexing 1259/1259" plus "viewing" took the whole row and
// left the repository's name as "k…". Compact says the same thing in the
// fewest characters (a percentage, or "error") and moves the full wording into
// the tooltip, so the name is the last thing on the row to give way.
export function RepoIndexChip({ repo, compact }: {
  repo: Pick<RepoInfo, 'index_state' | 'index_done' | 'index_total' | 'index_reason'>;
  compact?: boolean;
}) {
  const state = repo.index_state;
  if (state !== 'indexing' && state !== 'error') return null;

  if (state === 'error') {
    // The server's reason leads when it gave one — "indexing cancelled" is a
    // very different thing to read than a job that failed, and the chip alone
    // says the same "index error" for both.
    const why = 'The repository is there; search over it may be incomplete until it is rebuilt.';
    const detail = repo.index_reason
      ? `The background index did not finish: ${repo.index_reason}. ${why}`
      : `The background index did not finish. ${why}`;
    return (
      <span
        data-testid="repo-index-error"
        title={compact ? `index error — ${detail}` : detail}
        style={{ ...chip, ...errorChip }}
      >{compact ? 'error' : 'index error'}</span>
    );
  }

  // The counts are the heal's OWN, and absent until it has counted its work.
  // "0/0" would read as a claim about the repo rather than as a count not yet
  // taken, so an uncounted heal says "indexing" and no more.
  const done = repo.index_done ?? 0;
  const total = repo.index_total ?? 0;
  const counts = total > 0 ? ` ${done}/${total}` : '';
  const why = 'The background search index is still being built. Reads work, but may be incomplete until it finishes.';
  // Floored, so the chip never claims 100% while work remains.
  const pct = total > 0 ? `${Math.min(100, Math.floor((done * 100) / total))}%` : '';
  return (
    <span
      data-testid="repo-index-indexing"
      title={compact ? `indexing${counts} — ${why}` : why}
      style={chip}
    >{compact && pct ? pct : `indexing${counts}`}</span>
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
