import type { CSSProperties } from 'react';
import { repoAvailable } from './api';
import type { RepoInfo } from './api';

// RepoStateChip marks a repository that is registered but has no live store.
//
// Such a repo used to be invisible: a database that failed to open dropped its
// repo out of the API entirely, leaving one ERROR line in a log the user never
// sees and a repository that had, as far as every screen was concerned, ceased
// to exist. control.db now records what EXISTS independently of what opens, so
// the row survives — and this chip is what tells the reader the difference
// between a repo they can read and one that is only registered.
//
// It renders NOTHING for an active repo. A chip on every row saying "fine"
// would be the screen answering a question nobody asked, and would drown the
// one row where the answer matters.
//
// The state word is the chip; the server's `detail` is the hover text. The two
// are not interchangeable: "missing" is the class of failure (and the thing to
// scan a list for), while the detail is the sentence that says what to do about
// this one. Cramming the sentence into the chip would widen every row for text
// only one reader in a hundred needs at a glance.
export function RepoStateChip({ repo }: { repo: Pick<RepoInfo, 'state' | 'detail'> }) {
  if (repoAvailable(repo)) return null;
  const state = repo.state as string;
  return (
    <span
      data-testid={`repo-state-${state}`}
      // The word alone is not self-explanatory out of context ("missing" —
      // missing what?), so the title carries the server's detail and falls back
      // to a sentence rather than to nothing.
      title={repo.detail || `This repository is registered but has no store (${state}).`}
      style={chip}
    >{state}</span>
  );
}

// Amber, not red. The repository and its registration are intact; what is
// broken is this process's ability to open it, and several of these states
// (a missing file that gets restored, a conflict that is resolved elsewhere)
// clear themselves without anything being lost. Red is for data that is gone.
const chip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22',
};
