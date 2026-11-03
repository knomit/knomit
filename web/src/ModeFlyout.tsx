import { useEffect, useRef, useState } from 'react';
import type { ComponentType, KeyboardEvent as ReactKeyboardEvent, PointerEvent as ReactPointerEvent } from 'react';

type IconType = ComponentType<{ color: string; size?: number }>;

/** One mode button, already resolved by the caller (label, tooltip, state). */
export interface FlyoutItem {
  key: string;
  testid: string;
  ariaLabel: string;
  title: string;
  Icon: IconType;
  active: boolean;
  disabled: boolean;
  color: string;
  onSelect: () => void;
}

interface Props {
  /** Every visible mode in the header's order. The current one is drawn at the
   *  right, the others in this order inline to its left. */
  items: FlyoutItem[];
  currentKey: string;
  /** The current icon is a trigger, not an action — EXCEPT for a derived mode
   *  (a search's relevance, a motif pivot), whose lit segment has always been
   *  the way OUT. While it is current every other mode is disabled, so the
   *  flyout has nothing to pick and the click keeps that exit. */
  currentActs: boolean;
}

/** Collapse delay after the pointer leaves the group (book design §5). */
export const COLLAPSE_MS = 250;

/**
 * The list header's modes as a Photoshop-style tool group (book design §5,
 * prototype item 1, option (d)): at rest only the CURRENT mode's icon, with a
 * 4px corner triangle hinting at the group; hover or keyboard focus opens the
 * others INLINE TO THE LEFT in an opaque strip over whatever is there; one
 * click picks. It collapses COLLAPSE_MS after the pointer leaves (moving
 * between icons never leaves: the strip is a DOM child of the group), on Esc
 * (focus back on the current icon) and when focus leaves the group. A click on
 * the current icon keeps it open for a mouse and toggles it for touch or pen.
 * Arrow keys move between modes while open, in on-screen order; only the
 * current icon is in the Tab order.
 */
export function ModeFlyout({ items, currentKey, currentActs }: Props) {
  const [open, setOpen] = useState(false);
  const groupRef = useRef<HTMLDivElement>(null);
  const currentRef = useRef<HTMLButtonElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Set while WE move focus back to the current icon, so that focus does not
  // re-open what we are closing.
  const quietFocus = useRef(false);
  // Touch: focus (which opens) lands between pointerdown and click, so the
  // toggle must read the state from BEFORE the press.
  const press = useRef<{ type: string; wasOpen: boolean } | null>(null);
  // Set from a pointerdown inside the group until the pointer is released.
  // Safari blurs a pressed button with a null relatedTarget BEFORE its click;
  // collapsing then would unmount the strip and lose the click.
  const pressing = useRef(false);

  const cancel = () => { if (timer.current) { clearTimeout(timer.current); timer.current = null; } };
  useEffect(() => cancel, []);

  const current = items.find(i => i.key === currentKey) ?? items[0];
  const others = items.filter(i => i !== current);

  const focusCurrent = () => {
    quietFocus.current = true;
    currentRef.current?.focus();
    quietFocus.current = false;
  };
  const collapse = (refocus: boolean) => {
    cancel();
    setOpen(false);
    if (refocus) focusCurrent();
  };

  const onCurrentClick = () => {
    const p = press.current;
    press.current = null;
    pressing.current = false;
    if (currentActs) { if (!current.disabled) current.onSelect(); collapse(false); return; }
    if (p && (p.type === 'touch' || p.type === 'pen')) setOpen(!p.wasOpen);
    else setOpen(true);
  };

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    // The Library's window-level list keys run off keys pressed anywhere but a
    // text field. Enter and Space belong to the focused icon (the Library's
    // Enter preventDefaults, which would cancel the button's activation).
    // Left/Right belong to the flyout only while it is OPEN; closed, they and
    // Up/Down stay the list's, so the arrows keep moving the selection after
    // a pick leaves focus on the current icon.
    if (e.key === 'Enter' || e.key === ' ') e.stopPropagation();
    if (open && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) e.stopPropagation();
    if (e.key === 'Escape' && open) {
      e.stopPropagation();
      collapse(true);
      return;
    }
    if (!open || (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight')) return;
    e.preventDefault();
    // On-screen order: the strip left to right, then the current icon.
    const btns = Array.from(groupRef.current?.querySelectorAll<HTMLButtonElement>('button[data-flyout-btn]') ?? [])
      .filter(b => !b.disabled);
    const at = btns.indexOf(document.activeElement as HTMLButtonElement);
    const next = e.key === 'ArrowLeft' ? Math.max(0, at - 1) : Math.min(btns.length - 1, at + 1);
    if (at === -1 || next === at) return;
    btns[next].focus();
  };

  const btnStyle = (it: FlyoutItem): React.CSSProperties => ({
    background: 'none', color: it.color, border: 'none', outline: 'none',
    padding: '3px 5px', borderRadius: 4,
    cursor: it.disabled ? 'not-allowed' : 'pointer',
    display: 'inline-flex', alignItems: 'center', position: 'relative',
  });
  const hover = (it: FlyoutItem) => ({
    onMouseEnter: (e: React.MouseEvent<HTMLButtonElement>) => { if (!it.disabled && !it.active) e.currentTarget.style.color = '#aaa'; },
    onMouseLeave: (e: React.MouseEvent<HTMLButtonElement>) => { if (!it.disabled && !it.active) e.currentTarget.style.color = it.color; },
  });

  return (
    <div
      ref={groupRef}
      data-testid="mode-flyout"
      data-open={open ? 'true' : 'false'}
      role="group"
      style={{ position: 'relative', display: 'inline-flex', flexShrink: 0 }}
      onMouseEnter={() => { cancel(); setOpen(true); }}
      onMouseLeave={() => { cancel(); timer.current = setTimeout(() => { timer.current = null; setOpen(false); }, COLLAPSE_MS); }}
      onFocus={() => { if (!quietFocus.current) { cancel(); setOpen(true); } }}
      onPointerDownCapture={() => {
        pressing.current = true;
        const release = () => { pressing.current = false; };
        window.addEventListener('pointerup', release, { once: true, capture: true });
        window.addEventListener('pointercancel', release, { once: true, capture: true });
      }}
      onBlur={e => {
        if (pressing.current) return;
        const to = e.relatedTarget as Node | null;
        if (!to || !groupRef.current?.contains(to)) collapse(false);
      }}
      onKeyDown={onKeyDown}
    >
      {open && others.length > 0 && (
        <div
          data-testid="mode-flyout-strip"
          style={{
            position: 'absolute', right: '100%', top: 0, bottom: 0,
            display: 'flex', alignItems: 'center', gap: 4, paddingLeft: 4,
            background: '#0f0f0f', zIndex: 2,
          }}
        >
          {others.map(it => (
            <button
              key={it.key}
              type="button"
              data-flyout-btn=""
              data-testid={it.testid}
              tabIndex={-1}
              disabled={it.disabled}
              aria-label={it.ariaLabel}
              aria-pressed={it.active}
              title={it.title}
              onClick={e => {
                pressing.current = false;
                if (it.disabled) return;
                it.onSelect();
                // A keyboard pick (detail 0) keeps focus in the group; a mouse
                // pick does not pull focus back onto the current icon.
                collapse(e.detail === 0);
              }}
              {...hover(it)}
              style={btnStyle(it)}
            ><it.Icon color="currentColor" size={14} /></button>
          ))}
        </div>
      )}
      <button
        ref={currentRef}
        type="button"
        data-flyout-btn=""
        data-testid={current.testid}
        data-current="true"
        tabIndex={0}
        disabled={current.disabled}
        aria-label={current.ariaLabel}
        aria-pressed={current.active}
        aria-haspopup="true"
        aria-expanded={open}
        title={current.title}
        onPointerDown={(e: ReactPointerEvent<HTMLButtonElement>) => { press.current = { type: e.pointerType, wasOpen: open }; }}
        onClick={onCurrentClick}
        {...hover(current)}
        style={btnStyle(current)}
      >
        <current.Icon color="currentColor" size={14} />
        {/* The group hint: a 4px triangle in the bottom-right corner. */}
        <span
          data-testid="mode-flyout-hint"
          aria-hidden="true"
          style={{
            position: 'absolute', right: 1, bottom: 1, width: 0, height: 0,
            borderLeft: '4px solid transparent', borderBottom: '4px solid currentColor',
          }}
        />
      </button>
    </div>
  );
}
