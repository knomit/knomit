import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { api } from './api';
import type { CommitDetail, CommitFile } from './api';
import { CHAT_RESULT_LOADING, COMMIT_FACTS_AFFECTED } from './chatCopy';
import { NoticeLine } from './ChatNotice';
import { CommitBlock } from './TimelineNav';

/** Which of a commit's files are facts. The server already lists only what is
 *  under the ontology root, so `.knomit/` and `artifacts/` never arrive; what
 *  can is a non-fact under that root (kb/.drafts/, closed to the fact reads, or
 *  a file that is not markdown), whose row would open nothing. */
const factFiles = (files: CommitFile[]): CommitFile[] => files.filter(f =>
  f.path.endsWith('.md') && !f.path.split('/').some(seg => seg.startsWith('.')));

/** The server's sentence, not the request URL fetchJSON puts in front of it. */
function PanelLoadError({ error, testId }: { error: string; testId: string }) {
  const sentence = /→ \d{3} ([\s\S]+)$/.exec(error)?.[1] ?? error;
  return <div role="alert" style={{ padding: '0 18px' }}><NoticeLine tone="blocked" testId={testId} lead={sentence} /></div>;
}

/** The panel's record of the commits it has shown, by repo, branch and hash: the
 *  answer (a commit does not change) and how far its list was scrolled. */
export type CommitMemo = Map<string, { detail: CommitDetail; scrollTop: number }>;

/**
 * ChatCommitView shows one commit in the chat's side panel: the timeline's own
 * commit block (CommitBlock), open, listing the facts it affected. Clicking one
 * opens it in the same panel, in the fact view, as of this commit (a fact the
 * commit retracted reads as its last version there, as in Browse); the commit's
 * chip in the answer returns here.
 *
 * The panel unmounts this view while a fact is on screen, so `memo` holds what it
 * needs to return to the same place in a long list without a second read.
 */
export function ChatCommitView({ repo, branch, commit, memo, onOpenFact }: {
  repo: string;
  branch: string;
  commit: string;
  memo: CommitMemo;
  onOpenFact: (path: string) => void;
}) {
  const key = `${repo}\0${branch}\0${commit}`;
  const [loaded, setLoaded] = useState<{ key: string; detail?: CommitDetail; error?: string } | null>(null);
  useEffect(() => {
    if (memo.has(key)) return;
    let cancelled = false;
    api.commitDetail(repo, branch, commit)
      .then(detail => {
        memo.set(key, { detail, scrollTop: 0 });
        if (!cancelled) setLoaded({ key, detail });
      })
      .catch(e => { if (!cancelled) setLoaded({ key, error: e instanceof Error ? e.message : String(e) }); });
    return () => { cancelled = true; };
  }, [repo, branch, commit, key, memo]);

  const remembered = memo.get(key);
  const shown = remembered ? { key, detail: remembered.detail, error: undefined } : loaded?.key === key ? loaded : null;
  // Before paint, so a returning reader never sees the top of the list first.
  const box = useRef<HTMLDivElement>(null);
  const hasDetail = !!shown?.detail;
  useLayoutEffect(() => {
    if (box.current) box.current.scrollTop = memo.get(key)?.scrollTop ?? 0;
  }, [key, hasDetail, memo]);
  const [detailOpen, setDetailOpen] = useState(true);
  if (!shown) return <div data-testid="chat-commit-loading" style={{ color: '#777', fontSize: 13, padding: '14px 18px' }}>{CHAT_RESULT_LOADING}</div>;
  if (!shown.detail) return <PanelLoadError error={shown.error ?? ''} testId="chat-commit-error" />;
  const { detail } = shown;
  return (
    <div ref={box} data-testid="chat-commit" onScroll={e => { const m = memo.get(key); if (m) m.scrollTop = e.currentTarget.scrollTop; }} style={{ height: '100%', minHeight: 0, overflowY: 'auto', padding: '4px 0', boxSizing: 'border-box' }}>
      <CommitBlock
        commit={detail.commit || commit}
        operation={detail.operation}
        date={detail.date}
        message={detail.message}
        active
        onRowClick={() => setDetailOpen(o => !o)}
        detail={detail}
        detailOpen={detailOpen}
        filesLabel={COMMIT_FACTS_AFFECTED}
        files={factFiles(detail.files)}
        onOpenFile={f => onOpenFact(f.path)}
      />
    </div>
  );
}
