import { useState, useEffect, useRef } from 'react';

/**
 * The collapsed "…" standing in for hidden middle segments; opens a dropdown
 * listing them (in order) so any can be jumped to directly.
 *
 * SHARED by TrailBreadcrumb (time-travel crumbs) and LibraryHeader (path
 * ancestors), and it lives here rather than in either of them because its
 * dismiss behaviour — outside-click, Escape, and the listener cleanup that stops
 * a closed menu leaking handlers — is the part that is easy to get subtly wrong
 * and is already tested. A second copy would fork that, and the copy is always
 * the one that misses the cleanup.
 *
 * `indices` are indices into the CALLER's full array, and are handed back to
 * onJump untouched. Neither caller may pass indices into a collapsed layout.
 */
export function OverflowCrumb({ indices, label, onJump }: {
  indices: number[];
  label: (index: number) => string;
  onJump: (index: number) => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('mousedown', onDown);
    return () => {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('mousedown', onDown);
    };
  }, [open]);

  return (
    <span ref={ref} style={{ position: 'relative', display: 'inline-flex', alignItems: 'center' }}>
      <button
        data-testid="crumb-overflow"
        onClick={() => setOpen(o => !o)}
        title={`${indices.length} more`}
        aria-haspopup="menu"
        aria-expanded={open}
        style={{
          cursor: 'pointer', background: 'none', border: 'none',
          padding: '1px 4px', fontSize: 11.5, color: '#888', lineHeight: 1,
        }}
      >
        …
      </button>
      {open && (
        <div
          data-testid="crumb-overflow-menu"
          role="menu"
          style={{
            position: 'absolute', top: '100%', left: 0, marginTop: 4, zIndex: 30,
            background: '#161616', border: '1px solid #333', borderRadius: 6,
            padding: 4, minWidth: 180, maxWidth: 320,
            boxShadow: '0 6px 18px rgba(0,0,0,0.5)',
            maxHeight: 280, overflowY: 'auto',
          }}
        >
          {indices.map(index => (
            <button
              key={index}
              role="menuitem"
              onClick={() => { onJump(index); setOpen(false); }}
              title={label(index)}
              style={{
                display: 'block', width: '100%', textAlign: 'left',
                cursor: 'pointer', background: 'none', border: 'none',
                padding: '5px 8px', borderRadius: 4, fontSize: 11.5, color: '#bbb',
                whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
              }}
              onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = '#222'; }}
              onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = 'none'; }}
            >
              {label(index)}
            </button>
          ))}
        </div>
      )}
    </span>
  );
}
