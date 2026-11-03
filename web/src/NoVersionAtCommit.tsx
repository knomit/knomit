/**
 * The state a commit-anchored read lands in when the server answers 404
 * commit_not_in_repo: the open fact's repo does not hold the anchored commit.
 * In a lens this is the ordinary result of the mounts picker re-selecting a
 * fact on ANOTHER mount while a timeline is open: asOf keeps the previous
 * mount's commit (user ruling 2026-11-03: do not drop back to live). It is a
 * state, not an error, so it is not drawn in the error red, and history mode
 * stays on: the reader scrubs or returns to live from the timeline as usual.
 */
export function NoVersionAtCommit({ commit }: { commit: string }) {
  return (
    <div data-testid="fact-no-version-at-commit" style={{ padding: 24, color: '#888', fontSize: 13 }}>
      {/* PLACEHOLDER copy: the wording is the user's to settle. */}
      This fact has no version at commit{' '}
      <span style={{ fontFamily: 'var(--k-font-mono)', color: '#8af' }}>{commit.slice(0, 7)}</span>
      {' '}in this repo.
    </div>
  );
}
