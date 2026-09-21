import { useEffect, useState } from 'react';
import { api, ExperimentConflictError } from './api';
import type { ExperimentInfo } from './api';
import { FlaskIcon } from './icons';
import { changedSinceFork, expiryLong, relAge } from './experimentText';
import { ExperimentConflictDialog } from './ExperimentConflictDialog';

// ExperimentBand is the strip under the top bar, shown only while the window
// is inside an experiment.
//
// WHY A BAND AND NOT JUST THE CHIP. The chip says WHICH experiment; the band
// says what being in one costs and how to leave. Those are the facts a user
// needs while working — what it forked from, how much is in it, when it lapses
// — and the three lifecycle actions. Burying them in Manage meant every exit
// from an experiment began with "where was that panel again"; a mode you can
// enter from the top bar must be leavable from the top bar.
//
// It is deliberately NOT a second copy of the panel: no list, no description
// editing, no create. One experiment — the one you are in — and its three
// verbs.
interface Props {
  repo: string;
  /** The experiment branch the app is showing, i.e. `exp/<name>`. */
  branch: string;
  /** The branch row's experiment object. The band renders only when this exists. */
  experiment: ExperimentInfo;
  /** Switch the window to another branch — how commit and rollback get you out. */
  onEnterBranch: (branch: string) => void;
  /**
   * Called after any action that changed the experiment, so surfaces holding
   * their own copy of the list (the Manage panel) can re-read.
   */
  onChanged?: () => void;
}

export function ExperimentBand({ repo, branch, experiment, onEnterBranch, onChanged }: Props) {
  const [changed, setChanged] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [conflicts, setConflicts] = useState<string[] | null>(null);

  const parent = experiment.parent;

  // The changed count is READ FROM THE SAME FILTER the since-fork toggle uses,
  // asking for one row and keeping only the total, rather than being computed
  // here. Two counters that can disagree about what "changed since the fork"
  // means is how the band ends up contradicting the list right below it.
  //
  // The fetch lives IN the effect rather than in a loader called from it:
  // calling one would setState synchronously inside the effect body
  // (react-hooks/set-state-in-effect) and leave a resolved request able to
  // write into an unmounted band. Nothing re-reads in place — the two
  // remaining actions both leave the experiment.
  useEffect(() => {
    let alive = true;
    api.recent(repo, branch, '', '', 1, 0, { sinceFork: true })
      .then(res => { if (alive) setChanged(res.total); })
      // A count is decoration; failing to get one must not blank the band or
      // hide the actions, which are the part that matters.
      .catch(() => { if (alive) setChanged(null); });
    return () => { alive = false; };
  }, [repo, branch]);

  const run = async (action: 'commit' | 'rollback') => {
    setBusy(true);
    setError('');
    try {
      await api.experimentAction(repo, experiment.name, action);
      setConflicts(null);
      onChanged?.();
      // Both remaining actions END the experiment, so staying on its branch
      // would leave the window pointed at a ref that no longer exists.
      onEnterBranch(parent);
    } catch (e) {
      if (e instanceof ExperimentConflictError) setConflicts(e.paths);
      else setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const expiry = expiryLong(experiment.expires_at);
  const sep = <span style={{ color: '#3f5a4c' }}>·</span>;

  return (
    <>
      <div
        data-testid="experiment-band"
        data-experiment={experiment.name}
        style={{
          flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10,
          padding: '6px 14px', background: '#11201a',
          borderBottom: '1px solid #1f3a2c', fontSize: 12, color: '#9cb',
          flexWrap: 'wrap',
        }}
      >
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, color: '#7c9', fontWeight: 600, minWidth: 0 }}>
          <FlaskIcon color="currentColor" size={13} />
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{experiment.name}</span>
        </span>

        <span style={{ color: '#6a8' }}>
          forked from <span style={{ fontFamily: 'var(--k-font-mono, monospace)' }}>{parent}</span>{' '}
          {relAge(experiment.created_at)}
        </span>

        {changed !== null && <>{sep}<span data-testid="experiment-band-changed" style={{ color: '#6a8' }}>{changedSinceFork(changed)}</span></>}

        {/* No expiry segment at all when nothing expires: the server omits
            expires_at when expiry is off, and "never expires" is not news the
            user needs on every screen. */}
        {expiry && <>{sep}<span data-testid="experiment-band-expiry" style={{ color: '#6a8' }}>{expiry}</span></>}

        {error && (
          <>
            {sep}
            <span data-testid="experiment-band-error" style={{ color: '#f88' }}>{error}</span>
          </>
        )}

        <span style={{ flexGrow: 1 }} />

        <button data-testid="experiment-band-rollback" disabled={busy}
          title="Delete the experiment and everything on it. This cannot be undone."
          onClick={() => void run('rollback')} style={bandBtn(busy)}>
          Roll back
        </button>
        <button data-testid="experiment-band-commit" disabled={busy}
          title={`Merge into ${parent} and delete the experiment`}
          onClick={() => void run('commit')} style={bandBtn(busy, true)}>
          Commit
        </button>
      </div>

      {conflicts && (
        <ExperimentConflictDialog
          name={experiment.name}
          repo={repo}
          parent={parent}
          paths={conflicts}
          busy={busy}
          onClose={() => setConflicts(null)}
          onRollback={() => void run('rollback')}
        />
      )}
    </>
  );
}

// bandBtn is local rather than manageStyles.btn: those are sized for the
// Manage pane's 13px rows and would set the band's height on their own.
function bandBtn(disabled: boolean, primary = false): React.CSSProperties {
  return {
    font: 'inherit', fontSize: 12,
    padding: primary ? '3px 12px' : '3px 10px',
    borderRadius: 4,
    border: '1px solid ' + (primary ? '#3a7a5a' : '#2a4a3a'),
    background: disabled ? '#1a1a1a' : primary ? '#1e3a2c' : '#1a1a1a',
    color: disabled ? '#5a5a5a' : primary ? '#bfe' : '#aaa',
    fontWeight: primary ? 600 : 400,
    cursor: disabled ? 'default' : 'pointer',
    flexShrink: 0,
  };
}
