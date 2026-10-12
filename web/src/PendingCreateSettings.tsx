import { useEffect, useRef, useState } from 'react';
import { api, type RepoCreateStatus } from './api';
import { useRepoCreates, refreshRepoCreates, createRepoIsRegistered } from './useRepoCreates';
import { CreateProgress } from './CreateProgress';
import { btn } from './manageStyles';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';

// PendingCreateSettings is the PRE-REGISTRATION page: a create that has no
// repository yet, pinned from its rail row. Its only facet is Settings, and the
// top bar's label carries its chip (creating / cancelling / failed).
//
// Once the repo exists there is no separate page for it — the repository's
// own Settings renders in creating mode (see RepoSettings), which is the
// user's paradigm: one page gated by state, not a second page beside it. This
// one covers the window before m.Add, where there is genuinely nothing to show
// a repository page OF, and it hands over the moment there is (onOpenRepo).
//
// It reads the SHARED list rather than polling the single-job endpoint, so it
// cannot disagree with the rail and the footer about the same job, and costs
// no extra requests. A job the list no longer has — dismissed elsewhere, or
// aged out of the server's retention window — says so rather than spinning on
// a resource that is gone.
export function PendingCreateSettings({ createId, onClose, onOpenRepo }: {
  createId: string;
  /** The job was dismissed: the page has nothing left to show. */
  onClose: () => void;
  /** The job's repository now exists: show it. */
  onOpenRepo: (name: string) => void;
}) {
  const creates = useRepoCreates();
  const listed = creates.find(c => c.create_id === createId) ?? null;
  // The job the LIST no longer carries, read by id.
  //
  // A cancelled create is omitted from the collection on purpose — a
  // repository list has nothing left to say about it — so a page that read
  // only the list would lose the job at the exact moment the user is waiting
  // to be told what happened to it. The single-job resource outlives the
  // collection entry for this reason, and this is the client half of that
  // bargain. A job that is genuinely gone (dismissed, or aged past the
  // server's retention) fails this fetch and falls through to the "gone" note.
  const [byId, setById] = useState<RepoCreateStatus | null>(null);
  const inList = listed !== null;
  useEffect(() => {
    if (inList) return;
    let cancelled = false;
    api.getRepoCreate(createId).then(s => { if (!cancelled) setById(s); }).catch(() => {});
    return () => { cancelled = true; };
  }, [inList, createId]);
  // The fetched job is MATCHED BY ID rather than cleared when the page
  // switches creates, so a render between the switch and a clear can never
  // show the previous create's status under the new create's name.
  const status = listed ?? (byId?.create_id === createId ? byId : null);

  const [cancelErr, setCancelErr] = useState('');
  const [busy, setBusy] = useState(false);

  // THE PAGE IS OVER WHEN THE REPOSITORY EXISTS, not when the job ends.
  //
  // The job's terminal state arrives only after the whole index and the sync
  // activation — minutes, on a real repository, after the repo became
  // browsable. Guarded by the repo LIST, not by the status alone: this leaves
  // the create for the repository, and that is a claim the job's own word
  // cannot support. Never while cancelling — that job is deleting the very
  // repo this would open. The "already entered" latch is read in the effect,
  // never during render.
  const enteredRepo = useRef(false);
  const registered = status !== null
    && status.state !== 'cancelling' && status.state !== 'cancelled' && status.state !== 'failed'
    && createRepoIsRegistered(status);
  const registeredName = registered ? (status.repo?.name || status.name) : undefined;
  useEffect(() => {
    if (!registeredName || enteredRepo.current) return;
    let abandoned = false;
    void api.repos().then(list => {
      if (abandoned || enteredRepo.current) return;
      if (!list.some(r => r.name === registeredName)) return;
      enteredRepo.current = true;
      onOpenRepo(registeredName);
    }).catch(() => { /* a failed listing is not evidence the repo is there */ });
    return () => { abandoned = true; };
  }, [registeredName, onOpenRepo]);

  const cancel = async () => {
    setCancelErr(''); setBusy(true);
    try {
      await api.cancelRepoCreate(createId);
      await refreshRepoCreates();
    } catch (e) {
      setCancelErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const dismiss = async () => {
    try { await api.dismissRepoCreate(createId); } catch { /* the list tells the truth */ }
    await refreshRepoCreates();
    onClose();
  };

  const body = !status ? (
    <div data-testid="create-watch-gone" style={{ color: '#888', fontSize: 12 }}>
      This create is no longer listed — it was dismissed, or it finished long enough ago
      that the server has forgotten it. The repository list is the authoritative answer
      to whether it exists.
    </div>
  ) : status.state === 'cancelled' ? (
    <div data-testid="create-cancelled" style={{ fontSize: 13, color: '#9c9' }}>
      Create cancelled. No repository was added.
    </div>
  ) : (
    <>
      <div style={{ fontSize: 12, color: '#777', lineHeight: 1.6 }}>
        Browse and Activity appear when the repository exists and its first index finishes. You can leave
        this page; progress continues.
      </div>
      <CreateProgress status={status} />
      {/* THE OPERATIONS THAT APPLY, and only those. `cancel` is refused once
          the job is terminal, `dismiss` is refused while it is not. */}
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        {(status.state === 'running' || status.state === 'cancelling') && (
          <button type="button" data-testid="create-cancel-button"
            style={btn(busy || status.state === 'cancelling')}
            disabled={busy || status.state === 'cancelling'}
            onClick={() => { void cancel(); }}>
            {status.state === 'cancelling' ? 'Cancelling…' : 'Cancel create'}
          </button>
        )}
        {status.state === 'failed' && (
          <button type="button" data-testid="create-watch-dismiss" style={btn(false)}
            onClick={() => { void dismiss(); }}>Dismiss</button>
        )}
      </div>
      {cancelErr && (
        <div data-testid="create-cancel-error" style={{ fontSize: 12, color: '#e29a9a' }}>
          {cancelErr}
        </div>
      )}
    </>
  );

  const sections: Section[] = [{
    id: 'create',
    title: status?.name ? `Creating ${status.name}` : 'Creating',
    body: <div data-testid="create-watch" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>{body}</div>,
  }];
  return <SettingsPage kind="repo" sections={sections} testid="pending-create-settings" />;
}
