import { useCallback, useEffect, useState } from 'react';
import { api, ExperimentConflictError } from './api';
import type { ExperimentRow } from './api';
import { card, cardLabel, btn } from './manageStyles';
import { FlaskIcon } from './icons';
import { expiryShort, relAge } from './experimentText';
import { ExperimentConflictDialog } from './ExperimentConflictDialog';

// ExperimentsPanel is the repository page's view of this repo's experiments:
// enter one, commit it, throw it away.
//
// It is a CARD in the detail pane, structurally identical to the others there
// (manageStyles.card), because an experiment is a piece of this repository's
// configuration in exactly the way its remote and its licence are — not a
// separate mode with its own chrome.
//
// The rows are a TABLE, not stacked cards: the columns (name, description,
// last commit, expires) are the four things being compared across experiments,
// and a reader deciding which to throw away is scanning one column at a time.
//
// THERE IS NO CREATE CONTROL AND NO SYNC HERE (user rulings 2026-09-20).
// Experiments are opened by agents through the knomit_experiment MCP tool, and
// sync was removed because without a resolution step it only overwrites the
// experiment's version of the very facts a refused commit is about. What is
// left is browsing and housekeeping: Open, Commit, Roll back. The REST create
// and sync routes still exist (the mirrored routes must stay at parity); they
// simply have no caller in the UI, and the empty state says who opens one so
// the absence reads as a decision rather than a missing button.
//
// The expiry line is a POLICY STATEMENT, not a control. `experiments.expiry_days`
// is a server-level setting in knomit.toml and there is no config-write surface
// anywhere in knomit, so a control here would be a lie about what the UI can
// do. It reads the value from the API rather than hardcoding it, so it cannot
// drift from the sweeper that enforces it.

interface Props {
  repo: string;
  /** The branch the app is currently showing, so the row for it reads "you are here". */
  currentBranch: string;
  /** Switch the whole UI to a branch. Entering an experiment is just this. */
  onEnterBranch: (branch: string) => void;
  /** The repo's agent branch — where commit lands, and where leaving returns to. */
  agentBranch: string;
  /** A subscription has no agent branch and can hold no experiment. */
  disabledReason?: string;
}

const GRID = 'minmax(0, 1.5fr) minmax(0, 2.2fr) 110px 110px minmax(230px, auto)';

export function ExperimentsPanel({ repo, currentBranch, onEnterBranch, agentBranch, disabledReason }: Props) {
  const [rows, setRows] = useState<ExperimentRow[]>([]);
  const [expiryDays, setExpiryDays] = useState(0);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  // conflicts is the refused-commit state: the paths, kept beside the
  // experiment they belong to, because the repair is chosen by reading them.
  const [conflicts, setConflicts] = useState<{ name: string; paths: string[] } | null>(null);
  // `now` is captured when the list is read, not per render: every row then
  // ages against ONE instant (and Date.now() in render is impure — the
  // react-hooks/purity rule is right that a row's amber-or-grey must not flip
  // because something unrelated re-rendered).
  const [now, setNow] = useState(() => Date.now());

  const load = useCallback(async () => {
    try {
      const res = await api.listExperiments(repo);
      setRows(res.experiments);
      setExpiryDays(res.expiryDays);
      setNow(Date.now());
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoaded(true);
    }
  }, [repo]);

  // The disable below is a FALSE POSITIVE, not a waiver: every setState in
  // `load` sits after `await api.listExperiments(repo)` — including the catch
  // and finally arms — so none of them runs synchronously in this effect body,
  // which is the thing the rule forbids. It cannot see through the async
  // boundary. Deliberately NOT inlined the way ExperimentBand's fetch is:
  // `run()` calls `load` too, and one loader is what keeps the table and the
  // actions from disagreeing about what is in the repo.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => { void load(); }, [load]);

  const run = async (label: string, fn: () => Promise<unknown>, after?: () => void) => {
    setBusy(label);
    setError('');
    try {
      await fn();
      setConflicts(null);
      await load();
      after?.();
    } catch (e) {
      // A refused commit is a STATE with a repair, not an error message: the
      // paths are kept so the dialog can name them and say who resolves them.
      if (e instanceof ExperimentConflictError) setConflicts({ name: label.split(':')[1] ?? '', paths: e.paths });
      else setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy('');
    }
  };

  const rowBusy = busy !== '';

  return (
    <div style={card} data-testid="experiments-panel">
      <div style={cardLabel}>Experiments</div>

      <div style={{ fontSize: 12, color: '#8b9199', margin: '6px 0 10px', lineHeight: 1.5 }}>
        An experiment is a private branch of this knowledge base. Facts an agent writes inside
        one stay there until it is committed back.
      </div>

      {disabledReason && (
        <div data-testid="experiments-disabled" style={{
          fontSize: 12, color: '#c9a227', background: '#1f1b10',
          border: '1px solid #3a3320', borderRadius: 4, padding: '6px 8px', marginBottom: 10,
        }}>
          {disabledReason}
        </div>
      )}

      {error && (
        <div data-testid="experiments-error" style={{
          fontSize: 12, color: '#f88', background: '#2a1414',
          border: '1px solid #4a2222', borderRadius: 4, padding: '6px 8px', marginBottom: 10,
        }}>
          {error}
        </div>
      )}

      {!loaded && <div style={{ fontSize: 12, color: '#666' }}>Loading…</div>}

      {loaded && rows.length === 0 && (
        <div data-testid="experiments-empty" style={{ fontSize: 12, color: '#666', lineHeight: 1.5 }}>
          No experiments. An agent opens one with{' '}
          <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#aaa' }}>knomit_experiment</span>, which forks{' '}
          <span style={{ color: '#8af' }}>{agentBranch || 'the agent branch'}</span> so its work stays off that branch
          until it is committed. They are managed from here once they exist.
        </div>
      )}

      {loaded && rows.length > 0 && (
        <div style={{ border: '1px solid #2a2a2a', borderRadius: 6, background: '#1a1a1a', overflow: 'hidden' }}>
          <div style={{
            display: 'grid', gridTemplateColumns: GRID, gap: 12, padding: '6px 14px',
            fontSize: 10, letterSpacing: '.08em', textTransform: 'uppercase',
            color: '#555', borderBottom: '1px solid #262626',
          }}>
            <span>Name</span><span>Description</span><span>Last commit</span><span>Expires</span><span />
          </div>

          {rows.map((row, i) => {
            const here = row.branch === currentBranch;
            const writable = row.writable !== false;
            const expiring = (row.expires_at ? Date.parse(row.expires_at) - now : Infinity) < 10 * 86_400_000;
            return (
              <div
                key={row.name}
                data-testid={`experiment-row-${row.name}`}
                style={{
                  display: 'grid', gridTemplateColumns: GRID, gap: 12,
                  padding: '10px 14px', alignItems: 'center',
                  borderBottom: i === rows.length - 1 ? 'none' : '1px solid #262626',
                  background: here ? '#11201a' : 'transparent',
                }}
              >
                <div style={{ minWidth: 0 }}>
                  <div style={{
                    display: 'flex', alignItems: 'center', gap: 5, fontSize: 12.5,
                    fontWeight: here ? 600 : 400,
                    color: here ? '#7c9' : writable ? '#ddd' : '#8a8a8a',
                  }}>
                    <FlaskIcon color="currentColor" size={12} />
                    <span style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{row.name}</span>
                  </div>
                  {/* The subtitle carries the row's STATUS, which is the one
                      thing that changes what its buttons mean: where you are,
                      and whether this instance may still write it. */}
                  <div style={{ fontSize: 10, marginTop: 2, color: writable ? (here ? '#6a8' : '#666') : '#c9a227' }}>
                    {here && <span data-testid={`experiment-here-${row.name}`}>open in this window · </span>}
                    {!writable && <span data-testid={`experiment-orphan-${row.name}`}>orphaned · </span>}
                    forked from <span style={{ fontFamily: 'var(--k-font-mono, monospace)' }}>{row.parent}</span>
                    {!writable && ', not this instance’s agent branch'}
                  </div>
                </div>

                <div style={{ fontSize: 12, color: writable ? '#bbb' : '#777', lineHeight: 1.4, minWidth: 0 }}>
                  {row.description || ''}
                </div>

                <div style={{ fontSize: 12, color: writable ? '#aaa' : '#777' }}>{relAge(row.last_activity)}</div>

                <div style={{ fontSize: 12, color: expiring ? '#c9a227' : writable ? '#aaa' : '#777' }}>
                  {expiryShort(row.expires_at)}
                </div>

                <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
                  {/* An orphaned experiment still LISTS — it exists and can be
                      thrown away — but it cannot be written or committed, so
                      rollback is the one action it keeps. */}
                  {writable && !here && (
                    <button data-testid={`experiment-enter-${row.name}`} disabled={rowBusy}
                      onClick={() => onEnterBranch(row.branch)} style={btn(rowBusy, 'secondary')}>
                      Open
                    </button>
                  )}
                  <button data-testid={`experiment-rollback-${row.name}`} disabled={rowBusy}
                    title="Delete the experiment and everything on it. This cannot be undone."
                    onClick={() => void run(`rollback:${row.name}`, () => api.experimentAction(repo, row.name, 'rollback'), () => {
                      if (here) onEnterBranch(agentBranch);
                    })}
                    style={btn(rowBusy, 'danger')}>
                    Roll back
                  </button>
                  {/* Commit is offered only on the experiment you are IN.
                      It merges and then DELETES, so it is the one action whose
                      result you cannot see from here — and the mock the user
                      approved puts Open, not Commit, on the others. */}
                  {writable && here && (
                    <button data-testid={`experiment-commit-${row.name}`} disabled={rowBusy}
                      title={`Merge into ${agentBranch} and delete the experiment`}
                      onClick={() => void run(`commit:${row.name}`, () => api.experimentAction(repo, row.name, 'commit'), () => {
                        onEnterBranch(agentBranch);
                      })}
                      style={btn(rowBusy, 'primary')}>
                      Commit
                    </button>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}

      <div data-testid="experiments-policy" style={{ fontSize: 11, color: '#666', lineHeight: 1.5, marginTop: 10 }}>
        {expiryDays > 0
          ? <>Experiments expire after <span style={{ color: '#aaa' }}>{expiryDays} days</span> without a commit and are rolled back. Set <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#aaa' }}>experiments.expiry_days</span> in knomit.toml to change it; 0 means never.</>
          : <>Experiments never expire: <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#aaa' }}>experiments.expiry_days</span> is 0 in knomit.toml.</>}
      </div>

      {conflicts && (
        <ExperimentConflictDialog
          name={conflicts.name}
          parent={agentBranch}
          paths={conflicts.paths}
          busy={rowBusy}
          onClose={() => setConflicts(null)}
          onRollback={() => void run(`rollback:${conflicts.name}`, () => api.experimentAction(repo, conflicts.name, 'rollback'), () => {
            if (rows.find(r => r.name === conflicts.name)?.branch === currentBranch) onEnterBranch(agentBranch);
          })}
        />
      )}
    </div>
  );
}
