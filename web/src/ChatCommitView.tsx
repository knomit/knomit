import { useEffect, useState } from 'react';
import { api } from './api';
import type { CommitDetail, CommitFile } from './api';
import { CHAT_RESULT_LOADING } from './chatCopy';
import { PanelLoadError } from './ChatFactView';
import { CommitAuthorLine, CommitFileRow, CommitFilesLabel, CommitHash } from './TimelineNav';
import { relativeTime } from './utils';

/** Which of a commit's files are facts. The server already lists only what is
 *  under the ontology root, so `.knomit/` and `artifacts/` never arrive; what
 *  can is a non-fact under that root (kb/.drafts/, closed to the fact reads, or
 *  a file that is not markdown), whose row would open nothing. */
const factFiles = (files: CommitFile[]): CommitFile[] => files.filter(f =>
  f.path.endsWith('.md') && !f.path.split('/').some(seg => seg.startsWith('.')));

/**
 * ChatCommitView shows one commit in the chat's side panel: its hash and time,
 * its message and author, and the facts it affected, with the timeline's own
 * parts (TimelineNav). Clicking a fact opens it in the same panel as it read at
 * this commit, and the panel's Back returns here. A fact this commit retracted
 * has no version at the commit, so its row says so (struck through) and opens
 * the last version before it.
 */
export function ChatCommitView({ repo, branch, commit, onOpenFact }: {
  repo: string;
  branch: string;
  commit: string;
  onOpenFact: (path: string, removed: boolean) => void;
}) {
  const key = `${repo}\0${branch}\0${commit}`;
  const [loaded, setLoaded] = useState<{ key: string; detail?: CommitDetail; error?: string } | null>(null);
  useEffect(() => {
    let cancelled = false;
    api.commitDetail(repo, branch, commit)
      .then(detail => { if (!cancelled) setLoaded({ key, detail }); })
      .catch(e => { if (!cancelled) setLoaded({ key, error: e instanceof Error ? e.message : String(e) }); });
    return () => { cancelled = true; };
  }, [repo, branch, commit, key]);

  const shown = loaded?.key === key ? loaded : null;
  if (!shown) return <div data-testid="chat-commit-loading" style={{ color: '#777', fontSize: 13, padding: '14px 18px' }}>{CHAT_RESULT_LOADING}</div>;
  if (!shown.detail) return <PanelLoadError error={shown.error ?? ''} testId="chat-commit-error" />;
  const { detail } = shown;
  const facts = factFiles(detail.files);
  return (
    <div data-testid="chat-commit" style={{ height: '100%', minHeight: 0, overflowY: 'auto', padding: '16px 18px 20px', boxSizing: 'border-box' }}>
      {/* The hash and the time, as a timeline row sets them. */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 8 }}>
        <span data-testid="chat-commit-hash" title={commit} style={{ display: 'inline-flex' }}><CommitHash commit={detail.commit || commit} /></span>
        {detail.date && <span data-testid="chat-commit-date" title={detail.date} style={{ marginLeft: 'auto', fontSize: 9.5, color: '#555', flexShrink: 0 }}>{relativeTime(detail.date)}</span>}
      </div>
      {detail.message && (
        <div data-testid="chat-commit-message" style={{ fontFamily: 'var(--k-font-display)', fontSize: 17, fontWeight: 600, color: '#eee', marginBottom: 14, overflowWrap: 'anywhere' }}>
          {detail.message}
        </div>
      )}
      <CommitAuthorLine author={detail.author} />
      {facts.length > 0 && (
        <div style={{ marginTop: 14 }}>
          <CommitFilesLabel count={facts.length} />
          {facts.map(f => (
            <CommitFileRow key={f.path} file={f} onOpen={() => onOpenFact(f.path, f.action === 'deleted')} />
          ))}
        </div>
      )}
    </div>
  );
}
