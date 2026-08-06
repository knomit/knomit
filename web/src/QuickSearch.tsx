import { useRef } from 'react';

// QuickSearch is the dashboard's free-text search: a magnifier at the end of the
// stats strip that expands, in place, into a box you type in.
//
// It exists because the filter bar came OFF the summary panel — the bar's "+"
// picker offers the same domains, entities and types the facet columns below it
// already rank, and two ways to do one thing on one screen is one too many. But
// the bar is also the app's only free-text search, so removing it needed a way
// back that does not reintroduce what was removed.
//
// Hence a box, not a bar. There is nothing to pick here (the columns are the
// picker) and nothing to describe with chips until a search has returned
// something. The "+", the chips and the autocomplete all reappear on the fact
// view, where they have a result set to talk about.
//
// It expands INSIDE the strip rather than opening the bar's own slot above,
// because that slot is 34px the dashboard would have to slide down to make —
// moving the numbers the reader is looking at, at the exact moment they reach
// for the search. The strip is already this tall, so nothing moves.
//
// The query commits on ENTER, never per keystroke. Searching flips the list to
// relevance, which auto-opens the first result, which replaces this whole panel:
// on the bar's 300ms debounce that would happen mid-word and take the box's own
// surroundings with it. On enter, the query moves to the (now dressed) bar at
// the same moment the panel becomes the result view — one transition, not two.
export function QuickSearch({ open, onOpen, onClose, onSubmit }: {
  open: boolean;
  onOpen: () => void;
  onClose: () => void;
  onSubmit: (text: string) => void;
}) {
  // UNCONTROLLED, and deliberately so: the closed branch below unmounts the
  // input, so a fresh one mounts empty every time it opens. Reopening to find a
  // half-typed word still sitting there would read as a filter somehow still
  // applied — and holding the text in state would mean clearing it from an
  // effect, which is a cascading render for something the DOM does for free.
  // autoFocus lands the caret for the same reason.
  const inputRef = useRef<HTMLInputElement>(null);

  if (!open) {
    return (
      <button
        data-testid="quick-search-open"
        onClick={onOpen}
        title="Search facts  ( / )"
        aria-label="Search facts"
        style={{
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          width: 22, height: 22, flex: 'none', cursor: 'pointer',
          background: '#14171f', border: '1px solid #262c35', borderRadius: 4,
          color: '#7a8593', padding: 0, transition: 'color 0.12s, border-color 0.12s',
        }}
        onMouseEnter={e => { e.currentTarget.style.color = '#dfe6f0'; e.currentTarget.style.borderColor = '#39424f'; }}
        onMouseLeave={e => { e.currentTarget.style.color = '#7a8593'; e.currentTarget.style.borderColor = '#262c35'; }}
      >{magnifier(12)}</button>
    );
  }

  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', gap: 7, flex: 'none',
      width: 230, maxWidth: '40vw', padding: '2px 8px',
      background: '#161616', border: '1px solid #3a4553', borderRadius: 5,
      boxShadow: '0 0 0 2px rgba(136,170,255,0.07)',
    }}>
      <span aria-hidden style={{ color: '#8b95a6', opacity: 0.55, display: 'flex' }}>{magnifier(11)}</span>
      <input
        ref={inputRef}
        data-testid="quick-search-input"
        autoFocus
        defaultValue=""
        placeholder="Search facts…"
        aria-label="Search facts"
        onKeyDown={e => {
          if (e.key === 'Enter') {
            const q = (inputRef.current?.value || '').trim();
            if (q) onSubmit(q);
            return;
          }
          if (e.key === 'Escape') {
            // Window-level Escape clears every filter. Here it means "put the
            // box away", so it must not travel on and do both.
            e.stopPropagation();
            onClose();
          }
        }}
        style={{
          flex: 1, minWidth: 0, background: 'none', border: 0, outline: 'none',
          color: '#e8eef6', font: 'inherit', fontSize: 12, padding: 0,
        }}
      />
      <span
        data-testid="quick-search-close"
        role="button"
        aria-label="Close search"
        onClick={onClose}
        style={{ color: '#6b7482', fontSize: 13, lineHeight: 1, cursor: 'pointer', flex: 'none' }}
        onMouseEnter={e => { e.currentTarget.style.color = '#dfe6f0'; }}
        onMouseLeave={e => { e.currentTarget.style.color = '#6b7482'; }}
      >×</span>
    </span>
  );
}

function magnifier(size: number) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ display: 'block' }}>
      <circle cx="11" cy="11" r="8" /><line x1="21" y1="21" x2="16.65" y2="16.65" />
    </svg>
  );
}
