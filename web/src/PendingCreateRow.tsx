import type { CSSProperties } from 'react';
import type { RepoCreateStatus } from './api';

// PendingCreateRow renders a create job as a row in a repo list.
//
// A create takes minutes and produces nothing to list until it finishes, so
// every surface that shows repositories showed NOTHING about one in flight.
// The reported incident is exactly that: a user started a subscribe, navigated
// away, came back, and found no trace of it anywhere — while the clone was
// still running.
//
// The row is not a repository and does not pretend to be one. It is the work
// that will become one, with the state of that work on it, and it disappears
// when the repo it made appears in its place.
export function PendingCreateRow({ status, surface, onOpen, onDismiss, onCancel }: {
  status: RepoCreateStatus;
  /** Which list this row is in ('rail' | 'overview' | …). It only scopes the
   *  test ids: two surfaces legitimately show the same job at the same time,
   *  and one id for both makes them indistinguishable to anything that has to
   *  point at one of them. */
  surface?: string;
  /** Opens the create's own progress view. Absent for a finished job. */
  onOpen?: (createId: string) => void;
  /** Dismisses a FINISHED job. Absent while it is running — the server
   *  refuses that, and offering a dead control is worse than offering none. */
  onDismiss?: (createId: string) => void;
  /** Cancels a RUNNING create: stops the work and deletes anything it already
   *  produced. The exact complement of onDismiss — that one refuses a running
   *  job and never touches a repo, this one is only for a running job and the
   *  repo going away is the point — so the two are never offered together. */
  onCancel?: (createId: string) => void;
}) {
  const failed = status.state === 'failed';
  const running = status.state === 'running';
  // A cancellation is the outcome someone ASKED for, so it is neither a
  // failure nor a success and is drawn as neither: no error to read, no
  // progress bar for work that stopped, and no 'created' chip over a repo
  // that does not exist.
  const cancelled = status.state === 'cancelled';
  const key = surface ? `${surface}-${status.name}` : status.name;

  return (
    <div
      data-testid={`pending-create-${key}`}
      data-create-state={failed ? 'create-failed' : cancelled ? 'create-cancelled' : running ? 'creating' : 'created'}
      style={row}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
        <span style={{ ...name, opacity: failed || cancelled ? 0.7 : 1 }}>{status.name}</span>
        <span data-testid={`pending-create-chip-${key}`}
          style={failed ? failedChip : cancelled ? cancelledChip : creatingChip}>
          {failed ? 'create failed' : cancelled ? 'cancelled' : running ? 'creating' : 'created'}
        </span>
        {running && onOpen && (
          <button type="button" className="k-bare" style={linkBtn}
            data-testid={`pending-create-open-${key}`}
            onClick={() => onOpen(status.create_id)}>details</button>
        )}
        {running && onCancel && (
          <button type="button" className="k-bare" style={linkBtn}
            data-testid={`pending-create-cancel-${key}`}
            onClick={() => onCancel(status.create_id)}>cancel</button>
        )}
        {!running && onDismiss && (
          <button type="button" className="k-bare" style={linkBtn}
            data-testid={`pending-create-dismiss-${key}`}
            onClick={() => onDismiss(status.create_id)}>dismiss</button>
        )}
      </div>

      {failed ? (
        <div data-testid={`pending-create-error-${key}`} style={errorText}>
          {status.error || 'create failed'}
        </div>
      ) : cancelled ? (
        <div data-testid={`pending-create-cancelled-${key}`} style={messageText}>
          No repository was added.
        </div>
      ) : (
        <>
          <CreateBar status={status} surface={surface} />
          <div data-testid={`pending-create-message-${key}`} style={messageText}>
            {status.message || status.step || ''}
          </div>
        </>
      )}
    </div>
  );
}

// CreateBar draws the job's progress, and refuses to draw a percentage the
// server did not give it.
//
// `indeterminate` is the whole point. During the transfer nothing on the wire
// knows how many bytes a clone will move, so the server sends no percent and
// says so; a bar that filled to a made-up number there is what left the wizard
// apparently frozen at 40% for minutes. An indeterminate bar says "working,
// and I cannot tell you how far" — which is the true answer — while the
// message line beside it carries the remote's own progress text, which moves.
export function CreateBar({ status, surface }: { status: RepoCreateStatus; surface?: string }) {
  const pct = Math.max(0, Math.min(100, status.pct ?? 0));
  const key = surface ? `${surface}-${status.name}` : status.name;
  if (status.indeterminate) {
    return (
      <div data-testid={`create-bar-indeterminate-${key}`} aria-busy="true" style={track}>
        <div style={{ ...fill, ...sweep }} />
      </div>
    );
  }
  return (
    <div data-testid={`create-bar-${key}`}
      role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}
      style={track}>
      <div style={{ ...fill, width: `${pct}%` }} />
    </div>
  );
}

const row: CSSProperties = {
  display: 'flex', flexDirection: 'column', gap: 4,
  padding: '6px 10px', fontSize: 12,
};
const name: CSSProperties = {
  overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: '#ccc',
};
const chipBase: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};
const creatingChip: CSSProperties = {
  ...chipBase, color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
// Amber, not red: a failed create rolled itself back, so nothing is lost and
// nothing is half-made. The row exists to say why and to be dismissed.
const failedChip: CSSProperties = {
  ...chipBase, color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22',
};
// Grey, not amber: amber is the colour this row uses to say "read me, something
// did not go as asked". A cancellation went exactly as asked, so it recedes —
// the row is there to be dismissed, not attended to.
const cancelledChip: CSSProperties = {
  ...chipBase, color: '#8a8a8a', background: '#161616', border: '1px solid #2e2e2e',
};
const linkBtn: CSSProperties = {
  color: '#7a9ab5', fontSize: 11, padding: 0, background: 'none', border: 'none', cursor: 'pointer',
};
const errorText: CSSProperties = {
  color: '#c99', fontSize: 11, lineHeight: 1.5, wordBreak: 'break-word',
};
const messageText: CSSProperties = {
  color: '#7a7a7a', fontSize: 11, fontFamily: 'var(--k-font-mono)',
  overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const track: CSSProperties = {
  height: 3, borderRadius: 2, background: '#1e1e1e', overflow: 'hidden',
};
const fill: CSSProperties = {
  height: '100%', background: '#3d6b8a', transition: 'width 200ms linear',
};
// A fixed-width band rather than an animation: the CSS keyframes this would
// otherwise need live in App.css, and a bar that is visibly partial and does
// not claim a number reads correctly with or without motion.
const sweep: CSSProperties = { width: '35%', opacity: 0.8 };
