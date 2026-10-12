import { useState, useRef, useEffect } from 'react';
import type { CSSProperties } from 'react';
import { createPortal } from 'react-dom';
import type { AppState } from './state';
import { api } from './api';
import type { ExperimentRow } from './api';
import { useDismiss } from './hooks';
import { GitBranchIcon, ChevronDownIcon, FlaskIcon } from './icons';
import { shortBranch } from './utils';

interface Props {
  state: AppState;
  /** Switch the app to a branch of the current repo. Without it the chip is a
   *  passive label: a <button> that opens nothing would be a dead control. */
  onEnterBranch?: (branch: string) => void;
}

// BranchPicker is the Browse toolbar's branch chip and its menu: the agent
// branch, then this repo's experiments. It moved out of the top bar with the
// filter, because both act on the data being read and must not stay on screen
// over a page that reads none.
//
// The menu renders through a portal so the toolbar's overflow never clips it.
export function BranchPicker({ state, onEnterBranch }: Props) {
  const [open, setOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ top: 0, left: 0, minWidth: 0 });
  const [rows, setRows] = useState<ExperimentRow[]>([]);

  useDismiss(open, () => setOpen(false), [btnRef, menuRef]);

  // Fetched when the menu OPENS rather than held in app state: the list is
  // small, it is only ever read here, and a stale one would offer a branch
  // that a commit elsewhere has since deleted.
  useEffect(() => {
    if (!open || !state.repo) return;
    let alive = true;
    api.listExperiments(state.repo)
      .then(res => { if (alive) setRows(res.experiments); })
      .catch(() => { if (alive) setRows([]); });
    return () => { alive = false; };
  }, [open, state.repo]);

  if (!state.branch) return null;

  const toggle = () => {
    if (!open && btnRef.current) {
      const rect = btnRef.current.getBoundingClientRect();
      setPos({ top: rect.bottom + 6, left: rect.left, minWidth: rect.width });
    }
    setOpen(o => !o);
  };

  const pick = (branch: string) => {
    setOpen(false);
    if (branch && branch !== state.branch) onEnterBranch?.(branch);
  };

  // Inside an experiment the chip turns GREEN and names the experiment rather
  // than the branch. Green already means "write target" in this UI (the
  // Agent-branch card, the write mount tag), and inside an experiment that is
  // exactly what this branch is — so the marker is a change of state on the
  // chip the user already reads, not a second badge somewhere else.
  //
  // The chip does NOT say the word "experiment" (user ruling 2026-09-20): while
  // browsing, the name is the information and the label was noise on every
  // screen. The FLASK carries that meaning instead, and the word stays in
  // title/aria-label, where a hover or a screen reader still reaches it.
  //
  // The branch name is trimmed to the machine: identity.go builds agent
  // branches as agent/<host>-<fp8>, where the prefix is constant and the
  // fingerprint only separates two agents on one host. The title keeps the
  // whole thing.
  const interactive = Boolean(onEnterBranch);
  const label = state.experiment ? state.experiment.name : shortBranch(state.branch);
  const common = {
    'data-testid': 'toknomitr-branch',
    'data-experiment': state.experiment ? state.experiment.name : undefined,
    title: state.experiment
      ? `Experiment ${state.experiment.name} — forked from ${state.experiment.parent}`
      : state.branch,
    'aria-label': state.experiment
      ? `Experiment ${state.experiment.name}, forked from ${state.experiment.parent}`
      : undefined,
    style: {
      display: 'flex', alignItems: 'center', gap: 5, minWidth: 0, flexShrink: 0,
      fontSize: 12, fontFamily: 'inherit', lineHeight: 1.5,
      color: state.experiment ? '#7c9' : '#8af',
      background: state.experiment ? '#11201a' : 'transparent',
      border: '1px solid ' + (state.experiment ? '#2a4a3a' : 'transparent'),
      borderRadius: 3, padding: state.experiment ? '1px 6px' : '1px 2px',
      cursor: interactive ? 'pointer' : 'default',
    } as CSSProperties,
  };
  const inner = (
    <>
      {state.experiment
        ? <FlaskIcon color="currentColor" size={13} />
        : <GitBranchIcon color="currentColor" size={13} />}
      <span style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{label}</span>
      <ChevronDownIcon color="currentColor" size={11} />
    </>
  );

  // The agent branch is wherever an experiment says it forked from; outside
  // one it is the branch we are on. Derived rather than fetched: the row we
  // already hold names its own parent, which is the same value and one fewer
  // request.
  const agent = state.experiment ? state.experiment.parent : state.branch;
  const onAgent = !state.experiment;

  return (
    <>
      {interactive
        ? <button ref={btnRef} onClick={toggle} aria-haspopup="listbox" aria-expanded={open} {...common}>{inner}</button>
        : <span {...common}>{inner}</span>}

      {/* The agent branch, then this repo's experiments. Deliberately NOT a
          list of every branch: the consensus branch and other machines' agent
          branches are not writable, and offering them here would invite the
          user into a read-only view with no way to tell why from the menu. */}
      {open && createPortal(
        <div ref={menuRef} role="listbox" data-testid="toknomitr-branch-menu" style={{
          position: 'fixed', top: pos.top, left: pos.left,
          minWidth: Math.max(pos.minWidth, 240),
          background: '#1a1a1a', border: '1px solid #333', borderRadius: 6,
          zIndex: 10000, boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          padding: '5px 0', maxHeight: 340, overflowY: 'auto',
        }}>
          <div style={groupHeaderStyle}>
            <GitBranchIcon color="#6a8" size={11} /> Branch
          </div>
          <div
            role="option" aria-selected={onAgent}
            data-testid="toknomitr-branch-option-agent"
            onClick={() => pick(agent)}
            style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 12px', cursor: 'pointer', fontSize: 12, color: onAgent ? '#7c9' : '#aaa' }}
            onMouseEnter={e => { e.currentTarget.style.background = '#2a2a3a'; }}
            onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}
          >
            <span style={{ width: 10, color: '#7c9' }}>{onAgent ? '✓' : ''}</span>
            <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{shortBranch(agent)}</span>
            <span style={{ fontSize: 10, color: '#6a7078' }}>agent branch</span>
          </div>

          {rows.length > 0 && (
            <div style={groupHeaderStyle}>
              <FlaskIcon color="#6a8" size={11} /> Experiments
            </div>
          )}
          {rows.map(row => {
            const active = row.branch === state.branch;
            // An orphaned experiment is listed but NOT selectable: entering it
            // would land the user in a read-only view whose reason is not
            // visible from here. The repository page is where it can be
            // rolled back, which is its only remaining action.
            const usable = row.writable !== false;
            return (
              <div
                key={row.name} role="option" aria-selected={active}
                aria-disabled={!usable || undefined}
                data-testid={`toknomitr-branch-option-${row.name}`}
                title={usable ? row.description || undefined : `Forked from ${row.parent}, which is not this instance's agent branch`}
                onClick={usable ? () => pick(row.branch) : undefined}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '6px 12px',
                  cursor: usable ? 'pointer' : 'default', fontSize: 12,
                  color: active ? '#7c9' : usable ? '#aaa' : '#6a6a6a',
                }}
                onMouseEnter={e => { if (usable) e.currentTarget.style.background = '#2a2a3a'; }}
                onMouseLeave={e => { if (usable) e.currentTarget.style.background = 'transparent'; }}
              >
                <span style={{ width: 10, color: '#7c9' }}>{active ? '✓' : ''}</span>
                <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{row.name}</span>
                {!usable && <span style={{ fontSize: 10, color: '#c9a227' }}>orphaned</span>}
              </div>
            );
          })}

          {/* No "New experiment" entry: creation is MCP-only (user ruling
              2026-09-20), and no explanatory line when the list is empty —
              with no experiments the section is simply absent. The Branch
              group above always has the agent branch, so the menu is never
              empty. */}
        </div>,
        document.body,
      )}
    </>
  );
}

// Menu group header: small uppercase label with an icon.
const groupHeaderStyle: CSSProperties = {
  fontSize: 10, letterSpacing: '.1em', textTransform: 'uppercase', color: '#555',
  padding: '5px 12px 3px', display: 'flex', alignItems: 'center', gap: 6,
};
