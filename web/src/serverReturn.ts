// The server pages show OVER the target, and what stays mounted under them
// (Settings, the drafts) is display:none meanwhile, which drops every scroll
// position inside it and the focus. A ReturnPoint is taken just before they
// open and put back when they close.

export type ReturnPoint = {
  /** Every element that was scrolled, and how far. */
  scrolled: Array<[Element, number, number]>;
  /** Where focus goes back: the element the trip started from (the
   *  "Access → Secrets" link), else something in `within` (its picker). */
  focus: HTMLElement | null;
  within: HTMLElement | null;
};

/** takeReturnPoint records the scroll positions under `root`, and, when the
 *  trip starts from an element (`from`), where focus should come back to. */
export function takeReturnPoint(root: ParentNode, from?: HTMLElement | null): ReturnPoint {
  const scrolled: ReturnPoint['scrolled'] = [];
  root.querySelectorAll('*').forEach(el => {
    if (el.scrollTop > 0 || el.scrollLeft > 0) scrolled.push([el, el.scrollTop, el.scrollLeft]);
  });
  return { scrolled, focus: from ?? null, within: from?.closest<HTMLElement>('[data-return-focus]') ?? null };
}

/** restoreReturnPoint puts the scroll positions back on the elements still in
 *  the page, and the focus on `focus` if it is still there, else on the first
 *  control in `within`, else on `within` itself. */
export function restoreReturnPoint(p: ReturnPoint): void {
  for (const [el, top, left] of p.scrolled) {
    if (!el.isConnected) continue;
    el.scrollTop = top;
    el.scrollLeft = left;
  }
  if (p.focus?.isConnected) { p.focus.focus({ preventScroll: true }); return; }
  if (p.within?.isConnected) {
    const c = p.within.querySelector<HTMLElement>('select, button, input, textarea, [tabindex]:not([tabindex="-1"])');
    (c ?? p.within).focus({ preventScroll: true });
  }
}
