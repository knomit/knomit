// The materialize views' Focus mode (MaterializeView and FactSetView), in a
// module of its own: react-refresh/only-export-components keeps hooks out of a
// component file.

import { useEffect, useState } from 'react';

/** Input types in which Esc is a text-editing key (leave the field). A checkbox, radio,
 *  button, range and the like do not hold Esc: it exits Focus right away. */
const TEXT_INPUT_TYPES = new Set(['text', 'number', 'search', 'email', 'url', 'tel', 'password', 'date', 'time', 'datetime-local', 'month', 'week']);

/** Whether Esc, pressed with el focused, only leaves el (a text or number field, a textarea,
 *  an editable region) rather than exiting Focus. */
function holdsEsc(el: HTMLElement): boolean {
  if (el.isContentEditable) return true;
  if (el.tagName === 'TEXTAREA') return true;
  if (el.tagName === 'INPUT') return TEXT_INPUT_TYPES.has(((el as HTMLInputElement).type || 'text').toLowerCase());
  return false;
}

// ── Focus ───────────────────────────────────────────────────────────────────

/** Focus: opening a materialized view takes over the screen. The app shell's
 *  rail, library column, browse toolbar and facet tabs are hidden (CSS keyed on
 *  data-mat-focus, see materialize.css); the app's top bar and the article's own
 *  bar remain. ON by default; with it off NOTHING is hidden and the view only
 *  replaces the right-hand panel. Not remembered and not in the URL (it is how
 *  the reader chooses to read, not what is materialized). The toggle, Esc and
 *  Back end it; Back and leaving the view also restore everything. */
export function useMatFocus(): [boolean, (f: boolean | ((f: boolean) => boolean)) => void] {
  const [focus, setFocus] = useState(true);
  useEffect(() => {
    if (!focus) return;
    const root = document.documentElement;
    root.dataset.matFocus = '1';
    // Esc never reaches the shell while focus is on (its Escape clears the filters,
    // which would close the open fact and this view), so this listens first, in
    // the capture phase, and keeps the key to itself. In a text/number field or any
    // input it only leaves the field; anywhere else it exits Focus.
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopImmediatePropagation();
      const a = document.activeElement as HTMLElement | null;
      if (a && holdsEsc(a)) {
        a.blur();
        return;
      }
      setFocus(false);
    };
    window.addEventListener('keydown', onKey, true);
    return () => { delete root.dataset.matFocus; window.removeEventListener('keydown', onKey, true); };
  }, [focus]);
  return [focus, setFocus];
}

