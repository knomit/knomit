import { useRef, useState } from 'react';
import type { Dispatch } from 'react';
import type { Action } from './state';
import type { Lens } from './api';
import { useDismiss } from './hooks';
import { repoHue, LENS } from './utils';
import { LayersIcon, ChevronDownIcon } from './icons';

interface Props {
  lens: Lens;
  // Current sources-dropdown selection: null = all mounts, else the explicit
  // subset of read-mount repo names that are checked ([] = none selected).
  selection: string[] | null;
  dispatch: Dispatch<Action>;
}

// SourcesDropdown lets a lens reader narrow the union down to a subset of its
// read mounts. It lives in the left panel above the sort tabs and renders ONLY
// in lens context. Toggling a mount dispatches SET_LENS_SOURCES; a full set
// collapses back to null ("all mounts"), which drops the repos param so the
// server fans out to every mount.
export function SourcesDropdown({ lens, selection, dispatch }: Props) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  useDismiss(open, () => setOpen(false), [triggerRef, menuRef]);

  const mounts = lens.reads.map(r => r.repo);
  const total = mounts.length;
  // null selection means every mount is on; an explicit array means only those.
  const selected = new Set(selection === null ? mounts : selection);
  const filtered = selection !== null;
  const n = selection === null ? total : selection.length;

  const toggle = (repo: string) => {
    const next = new Set(selected);
    if (next.has(repo)) next.delete(repo); else next.add(repo);
    // Preserve server (reads) order and de-dupe by filtering the mount list.
    const arr = mounts.filter(m => next.has(m));
    // A full selection is semantically "all mounts" → collapse to null so the
    // repos param is dropped; an empty selection stays [] ("none selected").
    dispatch({ type: 'SET_LENS_SOURCES', repos: arr.length === total ? null : arr });
  };

  return (
    <div style={{ padding: '8px 12px', borderBottom: '1px solid #1a1a1a', background: '#0f0f0f' }}>
      <div style={{ fontSize: 10, letterSpacing: '.09em', textTransform: 'uppercase', color: '#555', marginBottom: 6 }}>
        Sources
      </div>
      {/* The trigger's wrapper is the menu's containing block. Without it the
          menu would resolve against whatever ancestor happens to be positioned
          — the left panel — and land somewhere else entirely. */}
      <div style={{ position: 'relative' }}>
      <div
        data-testid="sources-dropdown"
        ref={triggerRef}
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={() => setOpen(o => !o)}
        style={{
          display: 'flex', justifyContent: 'space-between', alignItems: 'center',
          background: '#161616', border: '1px solid #333', borderRadius: 5,
          padding: '5px 9px', fontSize: 12, color: '#bbb', cursor: 'pointer',
        }}
      >
        <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          <LayersIcon color={LENS.accent} size={12} />
          {filtered ? (
            <span data-testid="sources-label">{n} of {total} mounts</span>
          ) : (
            <span data-testid="sources-label">All mounts <span style={{ color: '#666' }}>· {total}</span></span>
          )}
        </span>
        <ChevronDownIcon color="#888" size={11} />
      </div>
      {open && (
        // Floats OVER the list rather than growing the panel. In flow it shoved
        // every row down by its own height, so opening the menu moved the rows
        // the reader was aiming at and each toggle re-laid-out the list beneath
        // it. Same anchoring the overflow-crumb menu uses.
        //
        // maxHeight because the left panel clips (overflow: hidden): a lens
        // with enough mounts to reach the panel's bottom edge would otherwise
        // have its last options cut off with no way to reach them.
        <div
          data-testid="sources-menu"
          ref={menuRef}
          style={{
            position: 'absolute', top: '100%', left: 0, right: 0, marginTop: 4, zIndex: 30,
            maxHeight: 260, overflowY: 'auto',
            background: '#1a1a1a', border: '1px solid #333', borderRadius: 5,
            padding: '4px 0', boxShadow: '0 6px 18px rgba(0,0,0,.45)',
          }}
        >
          {lens.reads.map(r => {
            const on = selected.has(r.repo);
            const c = repoHue(r.repo);
            return (
              <div
                key={r.repo}
                data-testid="source-option"
                data-repo={r.repo}
                data-on={String(on)}
                onClick={() => toggle(r.repo)}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '5px 10px',
                  fontSize: 12.5, color: on ? '#ddd' : '#888', cursor: 'pointer',
                }}
              >
                <span
                  aria-hidden
                  style={{
                    width: 14, height: 14, borderRadius: 3,
                    border: `1.5px solid ${on ? c : '#3a3a3a'}`,
                    background: on ? c : 'transparent',
                    display: 'flex', alignItems: 'center', justifyContent: 'center',
                    flexShrink: 0, color: '#0c0c0c', fontSize: 10, lineHeight: 1,
                  }}
                >
                  {on ? '✓' : ''}
                </span>
                <span style={{ width: 7, height: 7, borderRadius: '50%', background: c, flexShrink: 0 }} />
                <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.repo}</span>
              </div>
            );
          })}
        </div>
      )}
      </div>
    </div>
  );
}
