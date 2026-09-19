import type { CSSProperties } from 'react';
import { repoHue } from './utils';
import { createFlag } from './useRepoCreates';
import type { RepoCreateStatus } from './api';

// PendingCreateRow renders a create job as a row in a repo list.
//
// IT IS SHAPED LIKE A REPOSITORY ROW ON PURPOSE, and that is the whole design
// after the first attempt was rejected. A create is not a third kind of thing
// sitting next to repositories and lenses; it is a repository being made. The
// user's words: "I want to reuse the exact same UI paradigm, EXCEPT mark the
// repo as creating and show the current status and the appropriate operations
// in its details."
//
// So: the same hue dot, the same name treatment, the same click target, one
// chip where a repository's own chips go — and NO INLINE CONTROLS. The
// previous version put `details`, `cancel` and `dismiss` buttons on the row,
// which made it the only row in the rail with buttons on it, overflowed the
// rail's width, and read as "knomit [creating] [details] [cancel]" — the
// "all jumbled up and squished" the user reported. Clicking the row opens the
// create's page, precisely as clicking a repository opens its own.
export function PendingCreateRow({ status, surface, onOpen, style, active, disabled }: {
  status: RepoCreateStatus;
  /** Which list this row is in ('rail' | 'overview' | …). It only scopes the
   *  test ids: two surfaces legitimately show the same job at the same time,
   *  and one id for both makes them indistinguishable to anything that has to
   *  point at one of them. */
  surface?: string;
  /** Opens the create's own page. The row's ONLY action. */
  onOpen?: (createId: string) => void;
  /** The host list's own row style, so the rail's rows and this one are drawn
   *  by the same rule rather than by two that have to be kept in step. */
  style?: CSSProperties;
  /** Lit, exactly as a selected repository row is. */
  active?: boolean;
  disabled?: boolean;
}) {
  const flag = createFlag(status.state);
  if (!flag) return null;

  const key = surface ? `${surface}-${status.name}` : status.name;

  return (
    <button
      type="button"
      data-testid={`pending-create-${key}`}
      data-create-state={flag}
      style={style ?? defaultRow(!!active)}
      disabled={disabled}
      onClick={() => onOpen?.(status.create_id)}
    >
      <span style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
        {/* The repo's own deterministic hue, as everywhere else a repository is
            named. A create drawn without it would be the one row in the rail
            with no identity — which is exactly how it read as "not a repo". */}
        <span style={{ width: 8, height: 8, borderRadius: '50%', background: repoHue(status.name || '?'), flexShrink: 0 }} />
        <span data-testid={`pending-create-name-${key}`} style={name}>{status.name}</span>
      </span>
      {/* flexShrink: 0 on the chip and ellipsis on the name: the name yields,
          the flag never wraps, and the row cannot outgrow the rail however
          long the repository is called. */}
      <span data-testid={`pending-create-chip-${key}`} style={flag === 'failed' ? failedChip : chip}>
        {flag}
      </span>
    </button>
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
//
// It lives on the create's PAGE now, not on its list row. A list row that
// carried a progress bar was competing with the repository rows around it for
// a kind of attention none of them ask for.
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

// defaultRow mirrors RepoManager's listItem. Used only when a host list does
// not pass its own style — the rail and the overview both do, so their rows
// and these are drawn by one rule.
const defaultRow = (active: boolean): CSSProperties => ({
  width: '100%', display: 'flex', justifyContent: 'space-between', alignItems: 'center',
  background: active ? '#22303a' : 'transparent', color: active ? '#eee' : '#bbb',
  border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 13,
  cursor: 'pointer', textAlign: 'left',
});
const name: CSSProperties = {
  overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const chipBase: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};
const chip: CSSProperties = {
  ...chipBase, color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
// Amber, not red: a failed create rolled itself back, so nothing is lost and
// nothing is half-made. The row exists to say so and to be opened.
const failedChip: CSSProperties = {
  ...chipBase, color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22',
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
