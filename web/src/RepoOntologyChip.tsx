import type { CSSProperties } from 'react';
import type { RepoInfo } from './api';
import { LockIcon } from './icons';

// RepoOntologyChip marks an OPEN repository that has no usable ontology — none
// at any ontology path, an empty file, or one that does not parse.
//
// Such a repo mounts and reads normally, so nothing else on the row looks
// wrong; but every write to it is refused, and the reason used to be visible
// only in the server log. The chip says "read-only"; the server's own message
// is the hover text, because it names the file and the fault and this build
// cannot infer either.
//
// COMPACT is the manage rail's rule (see RepoIndexChip): the rail is a fixed
// column whose NAME is the point, so the chip shrinks to a lock and the word
// "read-only" moves into the tooltip with the message. The name is the last
// thing on the row to give way.
//
// It renders NOTHING when the server sent no ontology_error: a repo with an
// ontology, a repo being created, a row with no live store (RepoStateChip
// speaks for that one), and an older server all say nothing here.
export function RepoOntologyChip({ repo, compact }: {
  repo: Pick<RepoInfo, 'name' | 'ontology_error'>;
  compact?: boolean;
}) {
  if (!repo.ontology_error) return null;
  const title = `Read-only: this repository has no usable ontology, so every write is refused. ${repo.ontology_error}`;
  if (compact) {
    return (
      <span
        data-testid={`repo-ontology-error-${repo.name}`}
        role="img"
        aria-label="read-only"
        title={title}
        style={{ ...chip, padding: '1px 4px' }}
      ><LockIcon color="currentColor" size={10} /></span>
    );
  }
  return (
    <span data-testid={`repo-ontology-error-${repo.name}`} title={title} style={chip}>read-only</span>
  );
}

// Amber, like RepoStateChip: the data is intact and readable; what is missing
// is the taxonomy writes are validated against.
const chip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22',
};
