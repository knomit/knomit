import { useEffect, useRef, useState } from 'react';
import { api, type RepoCreateStatus } from './api';
import { useRepoCreates, refreshRepoCreates, createFlag, createRepoIsRegistered } from './useRepoCreates';
import { CreateProgress } from './CreateProgress';
import { repoHue } from './utils';
import { BookIcon } from './icons';
import { btn } from './manageStyles';
import { detailHead, headActions, repoIconBox } from './settingsStyles';

// LensSettings shows a lens's write target and its read mounts (with branch
// pins), a connect snippet, plus edit/delete actions. The lens object is passed
// from the parent's list (already fetched); getLens refreshes the full detail
// (e.g. to pull the description, which the list view may omit).
//
// The edit-mounts UI is built inline with the same style language as
// CreateLensForm (checkbox rows, LENS tokens) rather than importing its row
// component: that form's rows are tightly coupled to its own reads/branchData
// state, so extraction would force a risky refactor for no shared behavior.
// PendingCreateSettings is the PRE-REGISTRATION page: a create that has no
// repository yet.
//
// Once the repo exists there is no separate page for it — the repository's
// own details page renders in creating mode (see RepoSettings), which is the
// user's paradigm: one page gated by state, not a second page beside it. This
// one covers the window before m.Add, where there is genuinely nothing to show
// a repository page OF, and it hands over the moment there is.
//
// It reads the SHARED list rather than polling the single-job endpoint, so it
// cannot disagree with the rail and the indicator about the same job, and
// costs no extra requests. A job the list no longer has — dismissed elsewhere,
// or aged out of the server's retention window — says so rather than spinning
// on a resource that is gone.
export function PendingCreateSettings({ createId, onClose, onOpenRepo }: {
  createId: string;
  onClose: () => void;
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
  // server's retention) fails this fetch and falls through to the "gone" card.
  const [byId, setById] = useState<RepoCreateStatus | null>(null);
  const inList = listed !== null;
  useEffect(() => {
    if (inList) return;
    let cancelled = false;
    api.getRepoCreate(createId).then(s => { if (!cancelled) setById(s); }).catch(() => {});
    return () => { cancelled = true; };
  }, [inList, createId]);
  // The fetched job is MATCHED BY ID rather than cleared when the page
  // switches creates. Clearing it would be a setState inside the effect above,
  // and — worse — a render between the switch and that clear would show the
  // previous create's status under the new create's name. Carrying the id with
  // the answer makes the staleness impossible to render instead of merely
  // brief.
  const status = listed ?? (byId?.create_id === createId ? byId : null);

  const [cancelErr, setCancelErr] = useState('');
  const [busy, setBusy] = useState(false);

  // THE PAGE IS OVER WHEN THE REPOSITORY EXISTS, not when the job ends.
  //
  // The job's terminal state now arrives only after the whole index and the
  // sync activation — minutes, on a real repository, after the repo became
  // browsable. Waiting for it stranded the reader on a progress view of a
  // repository they could already open: "the only way to view the repo is to
  // refresh the page".
  //
  // Guarded by the repo LIST, not by the status alone, for the same reason the
  // wizard is: this leaves the create surface for the repository, and that is
  // a claim the job's own word cannot support. Never while cancelling — that
  // job is deleting the very repo this would open.
  // The "already entered" latch is read in the EFFECT, never during render:
  // a ref's value is not a render input, and reading one here would make this
  // component's output depend on something React does not track.
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

  const flag = status ? createFlag(status.state) : null;
  const nameOf = status?.name ?? '';

  return (
    <div data-testid="create-watch">
      {/* THE REPOSITORY PAGE'S OWN HEADER, not a variant of it: same icon box
          in the repo's hue, same name treatment, and the flag sitting exactly
          where a repository's subtitle sits. The user asked for "the exact
          same UI paradigm, EXCEPT mark the repo as creating" — so the only
          difference between this header and a real repository's is the word
          under the name. */}
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={repoIconBox(nameOf)}><BookIcon color={repoHue(nameOf)} size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>{nameOf || 'Create'}</h3>
            <div data-testid="create-watch-flag" style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              {status?.state === 'cancelling' ? 'cancelling'
                : status?.state === 'failed' ? 'create failed'
                  : status?.state === 'cancelled' ? 'create cancelled'
                    : flag ?? 'create'}
            </div>
          </div>
        </div>
        <div style={headActions}>
          <button type="button" className="k-bare" data-testid="create-watch-close"
            style={btnLink} onClick={onClose}>Back to overview</button>
        </div>
      </div>

      {!status ? (
        <div data-testid="create-watch-gone" style={{ color: '#888', fontSize: 12, marginTop: 12 }}>
          This create is no longer listed — it was dismissed, or it finished long enough ago
          that the server has forgotten it. The repository list is the authoritative answer
          to whether it exists.
        </div>
      ) : status.state === 'cancelled' ? (
        <div style={{ marginTop: 12 }}>
          {/* One way back, not two. The header already carries "Back to
              overview"; a second copy of the same link under the card was the
              page asking twice. */}
          <div data-testid="create-cancelled" style={{ fontSize: 13, color: '#9c9' }}>
            Create cancelled. No repository was added.
          </div>
        </div>
      ) : (
        <>
          <CreateProgress status={status} />

          {/* THE OPERATIONS THAT APPLY, and only those — the same rule a
              repository's page follows. A control that the server would refuse
              is worse than no control: `cancel` is refused once the job is
              terminal, `dismiss` is refused while it is not. */}
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 12 }}>
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

          {/* The "cancelling" copy is NOT repeated here: CreateProgress above
              already replaces its own headline with it, so this page and the
              wizard say it once, in the same words, from one place. */}
          {cancelErr && (
            <div data-testid="create-cancel-error" style={{ fontSize: 12, color: '#e29a9a', marginTop: 8 }}>
              {cancelErr}
            </div>
          )}
        </>
      )}
    </div>
  );
}

const btnLink: React.CSSProperties = {
  color: '#7a9ab5', fontSize: 12, background: 'none', border: 'none',
  cursor: 'pointer', padding: 0,
};
