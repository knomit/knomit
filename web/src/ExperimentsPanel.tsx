import { useCallback, useEffect, useState } from 'react';
import { api, ExperimentConflictError } from './api';
import type { ExperimentRow } from './api';
import { card, cardLabel, btn, confirmInput } from './manageStyles';
import { GitBranchIcon } from './icons';

// ExperimentsPanel is the repository page's view of this repo's experiments:
// open one, enter it, sync it, commit it, throw it away.
//
// It is a CARD in the detail pane, structurally identical to the others there
// (manageStyles.card), because an experiment is a piece of this repository's
// configuration in exactly the way its remote and its licence are — not a
// separate mode with its own chrome.
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

/** relAge renders an ISO timestamp as a short human age. */
function relAge(iso?: string): string {
  if (!iso) return '—';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '—';
  const mins = Math.max(0, Math.round((Date.now() - then) / 60000));
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

/** daysUntil renders the gap to an expiry date, or '' when there is none. */
function daysUntil(iso?: string): string {
  if (!iso) return '';
  const when = Date.parse(iso);
  if (Number.isNaN(when)) return '';
  const days = Math.ceil((when - Date.now()) / 86_400_000);
  if (days <= 0) return 'expires now';
  return `expires in ${days}d`;
}

export function ExperimentsPanel({ repo, currentBranch, onEnterBranch, agentBranch, disabledReason }: Props) {
  const [rows, setRows] = useState<ExperimentRow[]>([]);
  const [expiryDays, setExpiryDays] = useState(0);
  const [loaded, setLoaded] = useState(false);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  // conflicts is the refused-commit state: the paths, kept beside the
  // experiment they belong to, because the repair is chosen by reading them.
  const [conflicts, setConflicts] = useState<{ name: string; paths: string[] } | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await api.listExperiments(repo);
      setRows(res.experiments);
      setExpiryDays(res.expiryDays);
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoaded(true);
    }
  }, [repo]);

  useEffect(() => { void load(); }, [load]);

  const run = async (label: string, fn: () => Promise<unknown>, after?: () => void) => {
    setBusy(label);
    setError('');
    setConflicts(null);
    try {
      await fn();
      await load();
      after?.();
    } catch (e) {
      // A refused commit is a STATE with a repair, not an error message: the
      // paths are kept so the panel can offer sync-or-rollback against them.
      if (e instanceof ExperimentConflictError) setConflicts({ name: label.split(':')[1] ?? '', paths: e.paths });
      else setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy('');
    }
  };

  const canOpen = !disabledReason && name.trim() !== '' && busy === '';

  return (
    <div style={card} data-testid="experiments-panel">
      <div style={cardLabel}>Experiments</div>

      <div style={{ fontSize: 12, color: '#8b9199', marginBottom: 10, lineHeight: 1.5 }}>
        An experiment is a private branch of this knowledge base. Facts you write inside one
        stay there until you commit them back.{' '}
        <span data-testid="experiments-policy" style={{ color: '#6a7078' }}>
          {expiryDays > 0
            ? `Experiments with no commits for ${expiryDays} days are rolled back automatically.`
            : 'Experiments never expire automatically.'}
        </span>
      </div>

      {disabledReason && (
        <div data-testid="experiments-disabled" style={{
          fontSize: 12, color: '#c9a227', background: '#1f1b10',
          border: '1px solid #3a3320', borderRadius: 4, padding: '6px 8px', marginBottom: 10,
        }}>
          {disabledReason}
        </div>
      )}

      {/* New experiment. Two fields and one button: the name is the only
          required input, and the description is stored locally and never
          committed to git. */}
      {!disabledReason && (
        <div style={{ display: 'flex', gap: 6, marginBottom: 12, flexWrap: 'wrap' }}>
          <input
            data-testid="experiment-name"
            value={name}
            onChange={e => setName(e.target.value)}
            placeholder="new-experiment-name"
            aria-label="New experiment name"
            style={{ ...confirmInput, width: 200, flex: '0 1 200px' }}
          />
          <input
            data-testid="experiment-description"
            value={description}
            onChange={e => setDescription(e.target.value)}
            placeholder="what is it for? (optional)"
            aria-label="New experiment description"
            style={{ ...confirmInput, flex: '1 1 200px', minWidth: 140 }}
          />
          <button
            data-testid="experiment-open"
            disabled={!canOpen}
            onClick={() => void run('open', () => api.openExperiment(repo, name.trim(), description.trim()), () => {
              setName('');
              setDescription('');
            })}
            style={btn(!canOpen, 'primary')}
          >
            Open
          </button>
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

      {conflicts && (
        <div data-testid="experiment-conflicts" style={{
          fontSize: 12, color: '#e6b800', background: '#1f1b10',
          border: '1px solid #3a3320', borderRadius: 4, padding: '8px 10px', marginBottom: 10,
        }}>
          <div style={{ marginBottom: 4 }}>
            Commit refused: the agent branch and this experiment both changed{' '}
            {conflicts.paths.length === 1 ? 'a fact' : `${conflicts.paths.length} facts`}. Nothing changed.
          </div>
          <ul style={{ margin: '4px 0 6px 16px', padding: 0, color: '#c9a227' }}>
            {conflicts.paths.map(p => <li key={p} style={{ fontFamily: 'monospace', fontSize: 11 }}>{p}</li>)}
          </ul>
          <div style={{ color: '#8b9199' }}>
            Sync to take the agent branch’s version of those facts, then commit again — or roll the experiment back.
          </div>
        </div>
      )}

      {!loaded && <div style={{ fontSize: 12, color: '#666' }}>Loading…</div>}
      {loaded && rows.length === 0 && (
        <div data-testid="experiments-empty" style={{ fontSize: 12, color: '#666' }}>
          No experiments. Opening one forks <span style={{ color: '#8af' }}>{agentBranch || 'the agent branch'}</span> and
          lets you work without touching it.
        </div>
      )}

      {rows.map(row => {
        const here = row.branch === currentBranch;
        const writable = row.writable !== false;
        const rowBusy = busy !== '' ;
        return (
          <div
            key={row.name}
            data-testid={`experiment-row-${row.name}`}
            style={{
              display: 'flex', alignItems: 'flex-start', gap: 10,
              padding: '8px 10px', marginBottom: 6, borderRadius: 5,
              background: here ? '#11201a' : '#0f0f0f',
              border: '1px solid ' + (here ? '#2a4a3a' : '#242424'),
            }}
          >
            <div style={{ flex: 1, minWidth: 0 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                <GitBranchIcon color={here ? '#7c9' : '#8af'} size={12} />
                <span style={{ color: here ? '#7c9' : '#8af', fontSize: 12.5 }}>{row.branch}</span>
                {here && <span data-testid={`experiment-here-${row.name}`} style={{
                  fontSize: 10, textTransform: 'uppercase', letterSpacing: 1,
                  color: '#7c9', border: '1px solid #2a4a3a', borderRadius: 3, padding: '0 4px',
                }}>you are here</span>}
                {/* An orphaned experiment still LISTS — it exists and can be
                    thrown away — but it cannot be written or committed, and
                    saying which is the difference between a dead row and a
                    row with one remaining action. */}
                {!writable && <span data-testid={`experiment-orphan-${row.name}`} title={`Forked from ${row.parent}, which is not this instance's agent branch`} style={{
                  fontSize: 10, textTransform: 'uppercase', letterSpacing: 1,
                  color: '#c9a227', border: '1px solid #3a3320', borderRadius: 3, padding: '0 4px',
                }}>orphaned</span>}
              </div>
              {row.description && (
                <div style={{ fontSize: 12, color: '#9aa0a6', marginTop: 3 }}>{row.description}</div>
              )}
              <div style={{ fontSize: 11, color: '#6a7078', marginTop: 3 }}>
                last commit {relAge(row.last_activity)}
                {row.expires_at ? ` · ${daysUntil(row.expires_at)}` : ''}
              </div>
            </div>

            <div style={{ display: 'flex', gap: 5, flexShrink: 0, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
              {writable && !here && (
                <button data-testid={`experiment-enter-${row.name}`} disabled={rowBusy}
                  onClick={() => onEnterBranch(row.branch)} style={btn(rowBusy, 'secondary')}>
                  Enter
                </button>
              )}
              {writable && here && (
                <button data-testid={`experiment-leave-${row.name}`} disabled={rowBusy}
                  onClick={() => onEnterBranch(agentBranch)} style={btn(rowBusy, 'secondary')}>
                  Leave
                </button>
              )}
              {writable && (
                <>
                  <button data-testid={`experiment-sync-${row.name}`} disabled={rowBusy}
                    title="Bring the agent branch’s changes in. Where both changed a fact, the agent branch wins."
                    onClick={() => void run(`sync:${row.name}`, () => api.experimentAction(repo, row.name, 'sync'))}
                    style={btn(rowBusy, 'secondary')}>
                    Sync
                  </button>
                  <button data-testid={`experiment-commit-${row.name}`} disabled={rowBusy}
                    title={`Merge into ${agentBranch} and delete the experiment`}
                    onClick={() => void run(`commit:${row.name}`, () => api.experimentAction(repo, row.name, 'commit'), () => {
                      if (here) onEnterBranch(agentBranch);
                    })}
                    style={btn(rowBusy, 'primary')}>
                    Commit
                  </button>
                </>
              )}
              <button data-testid={`experiment-rollback-${row.name}`} disabled={rowBusy}
                title="Delete the experiment and everything on it. This cannot be undone."
                onClick={() => void run(`rollback:${row.name}`, () => api.experimentAction(repo, row.name, 'rollback'), () => {
                  if (here) onEnterBranch(agentBranch);
                })}
                style={btn(rowBusy, 'danger')}>
                Roll back
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}
