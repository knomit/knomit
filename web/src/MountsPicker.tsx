import { useRef, useState } from 'react';
import type { Dispatch } from 'react';
import type { Action } from './state';
import type { Lens } from './api';
import { useDismiss } from './hooks';
import { repoHue, LENS } from './utils';
import { LayersIcon, ChevronDownIcon } from './icons';

interface Props {
  lens: Lens;
  // Current selection: null = all mounts, else the explicit subset of read-mount
  // repo names that are checked ([] = none selected).
  selection: string[] | null;
  dispatch: Dispatch<Action>;
}

// Amber, not the resting blue. A narrowed scope is the silent explanation for a
// short list, so it cannot look like the state where nothing is being withheld.
const REST = '#8af';
const NARROWED = '#e5a23c';

// MountsPicker lets a lens reader narrow the union to a subset of its read
// mounts. It lives in the TOP BAR and renders only in lens context.
//
// It replaces two things that disagreed: a labelled dropdown in the left panel
// that knew the selection, and a top-bar chip rendering `lens.reads.length` —
// the total, always, whatever was selected. The chip was the more prominent of
// the two and the less true, so the control moved into its place.
//
// Toggling dispatches SET_LENS_SOURCES. A full set collapses back to null
// ("all mounts"), which drops the repos param so the server fans out to every
// mount rather than being handed each one by name.
export function MountsPicker({ lens, selection, dispatch }: Props) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  useDismiss(open, () => setOpen(false), [triggerRef, menuRef]);

  // Mount NAMES, not uids: the selection they drive is state.lensSources, which
  // the lens read endpoints take as repeated `repo=` params matched against the
  // binding's mount names. The uid identifies membership; the name addresses a
  // mount.
  const mounts = lens.reads.map(r => r.name);
  const total = mounts.length;
  const selected = new Set(selection === null ? mounts : selection);
  // A complete explicit selection means "all mounts" and must not read as a
  // filter — "3/3" claims a narrowing that is not there. The reducer collapses
  // these to null already; this is the guard for one arriving any other way.
  const narrowed = selection !== null && selection.length !== total;
  const n = selection === null ? total : selection.length;

  const setRepos = (repos: string[] | null) => dispatch({ type: 'SET_LENS_SOURCES', repos });

  const toggle = (repo: string) => {
    const next = new Set(selected);
    if (next.has(repo)) next.delete(repo); else next.add(repo);
    // Filtering the mount list rather than spreading the set preserves the
    // server's (reads) order no matter what order the reader clicked in.
    const arr = mounts.filter(m => next.has(m));
    setRepos(arr.length === total ? null : arr);
  };

  return (
    /* The trigger's wrapper is the menu's containing block. Without it an
       absolute menu resolves against whatever ancestor happens to be
       positioned — the whole top bar — and lands somewhere else entirely.

       data-nodrag belongs HERE rather than on the trigger alone, because this
       whole control sits inside the top bar's drag region and TopBar cancels
       the drag by walking `closest('[data-nodrag]')` from the click target. The
       menu is the trigger's SIBLING, so a tag on the trigger is not on any of
       its ancestors: mousedown on a mount option posted wails:drag and the
       native drag loop swallowed the click, leaving every option unclickable on
       the desktop build. --wails-draggable is the fallback for the same reason
       it is elsewhere in the bar — it inherits, and WKWebView ignores the
       -webkit-app-region property this used to set. */
    <div
      data-nodrag
      style={{
        position: 'relative', display: 'flex', alignItems: 'center',
        '--wails-draggable': 'no-drag',
      } as React.CSSProperties}
    >
      <div
        data-testid="mounts-picker"
        ref={triggerRef}
        role="button"
        tabIndex={0}
        aria-expanded={open}
        aria-label={narrowed ? `${n} of ${total} mounts` : `All ${total} mounts`}
        title={narrowed ? `Reading ${n} of ${total} mounts` : `Reading all ${total} mounts`}
        onClick={() => setOpen(o => !o)}
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 4,
          fontSize: 12, cursor: 'pointer', whiteSpace: 'nowrap',
          color: narrowed ? NARROWED : REST,
        }}
      >
        <LayersIcon color="currentColor" size={13} />
        <span data-testid="mounts-label">{narrowed ? `${n}/${total}` : String(total)}</span>
        <ChevronDownIcon color="currentColor" size={11} />
      </div>

      {open && (
        // Floats OVER the layout rather than growing it. In flow it shoved
        // every row below down by its own height, so opening the menu moved
        // the rows the reader was aiming at and each toggle re-laid-out the
        // list beneath it. maxHeight because a lens with enough mounts to
        // reach the viewport's bottom edge would otherwise have its last
        // options cut off with no way to reach them.
        <div
          data-testid="mounts-menu"
          ref={menuRef}
          style={{
            position: 'absolute', top: '100%', left: 0, marginTop: 5, zIndex: 60,
            minWidth: 186, maxHeight: 260, overflowY: 'auto',
            background: '#1a1a1a', border: '1px solid #333', borderRadius: 5,
            padding: '4px 0', boxShadow: '0 8px 22px rgba(0,0,0,.55)',
          }}
        >
          <div style={{
            display: 'flex', justifyContent: 'space-between', alignItems: 'center',
            padding: '4px 10px 5px', borderBottom: '1px solid #262626', marginBottom: 3,
            fontSize: 10, letterSpacing: '.09em', textTransform: 'uppercase', color: '#555',
          }}>
            <span>Mounts</span>
            <span style={{ display: 'flex', gap: 7, textTransform: 'none', letterSpacing: 0 }}>
              {/* Getting back to the whole union by clicking six checkboxes is
                  a chore the control should absorb. */}
              <span data-testid="mounts-all" onClick={() => setRepos(null)}
                style={{ color: LENS.accent, cursor: 'pointer', fontSize: 10.5 }}>all</span>
              <span data-testid="mounts-none" onClick={() => setRepos([])}
                style={{ color: '#666', cursor: 'pointer', fontSize: 10.5 }}>none</span>
            </span>
          </div>
          {lens.reads.map(r => {
            const on = selected.has(r.name);
            const c = repoHue(r.name);
            return (
              <div
                // Keyed by uid: it is the one part of a mount that cannot change
                // under the reader, so a rename re-labels a row instead of
                // remounting it.
                key={r.uid}
                data-testid="mount-option"
                data-repo={r.name}
                data-on={String(on)}
                onClick={() => toggle(r.name)}
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
                <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.name}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
