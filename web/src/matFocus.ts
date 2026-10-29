// Whether the materialize view's Focus mode is on, as the rest of the shell can
// read it. MaterializeView owns the state and publishes it as data-mat-focus on
// <html> (the stylesheet hides regions off that attribute); anything that must
// REACT to it in JS (the top bar's rail toggle goes inert) subscribes here.

import { useSyncExternalStore } from 'react';

export const MAT_FOCUS_ATTR = 'data-mat-focus';

export function isMatFocus(): boolean {
  return typeof document !== 'undefined' && document.documentElement.hasAttribute(MAT_FOCUS_ATTR);
}

function subscribe(cb: () => void): () => void {
  const mo = new MutationObserver(cb);
  mo.observe(document.documentElement, { attributes: true, attributeFilter: [MAT_FOCUS_ATTR] });
  return () => mo.disconnect();
}

export function useMatFocus(): boolean {
  return useSyncExternalStore(subscribe, isMatFocus, () => false);
}
