import { useRef, type KeyboardEvent } from 'react';
import { segGroup, segment, segDot, segSub, type SegTone } from './manageStyles';

// SegmentedChoice is the wizard's control for a step that settles ONE binary
// and then discloses what the answer means — StepSource (where the history
// lives) and StepReview (how to attach to a branch that is already a knowledge
// base). One component so the wizard asks both questions the same way, in
// behaviour as well as pixels: the styles were already shared, and sharing
// only the styles is what let the two call sites drift apart on semantics.
//
// It is a RADIOGROUP, not a toolbar of toggles. The difference is not
// cosmetic. A group of aria-pressed buttons — what both steps rendered before
// — announces each option as an independent toggle ("Join, toggle button, not
// pressed"), never says the two are mutually exclusive or that there are two
// of them, gives the group two tab stops instead of one, and leaves arrow keys
// doing nothing. role=radio + aria-checked inside role=radiogroup announces
// "Join, radio button, 1 of 2, not checked", which is what the control
// actually means.
//
// Arrow keys SELECT rather than merely moving focus. That is the native radio
// behaviour a screen-reader user expects, and it means a single arrow keypress
// fires onChange — including from a user who pressed it meaning to scroll,
// since preventDefault below stops the scroll.
//
// So the condition this control depends on is that SELECTING IS CHEAP: every
// onChange its call sites dispatch must be a pure field write that discards
// nothing. Both are — SET_ACCESS sets a field, and CHOOSE_LOCAL/CHOOSE_REMOTE
// now only set `choice` (CHOOSE_LOCAL used to null the probe as well, which
// made one arrow keypress silently destroy an established remote). Do NOT
// reuse this control for a lossy onChange: one that resets other state, fires
// a request, or navigates. Such a choice wants focus-only arrows with an
// explicit Space to commit, which is a different control.
export type SegmentedOption<T extends string> = {
  value: T;
  tone: SegTone;
  testid: string;
  /** The option's name — the thing the reader is choosing between. */
  title: string;
  /** One line saying what choosing it means. This is where a soft preference
   *  is carried (the led option describes itself more fully), never a badge. */
  sub: string;
};

export function SegmentedChoice<T extends string>({ label, value, onChange, options }: {
  /** Names the group to assistive tech ("How to attach"); the visible caption
   *  above the control is the step's own, and says the same thing. */
  label: string;
  value: T;
  onChange: (value: T) => void;
  options: SegmentedOption<T>[];
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const selected = options.findIndex(o => o.value === value);
  // ROVING TABINDEX: the group is one tab stop, and Tab lands on the option
  // that is currently checked. When `value` matches no option the first one
  // takes the tab stop instead — with a bare `checked ? 0 : -1` that case
  // gives every option tabIndex -1, and a keyboard user cannot reach the
  // control at all.
  const tabbable = selected < 0 ? 0 : selected;

  // Moves relative to `tabbable` — the CHECKED option — not to whichever
  // option happens to have focus. The two are the same thing here because
  // focus follows selection below and every call site accepts the change, so
  // the checked option is always the focused one. That assumption breaks if a
  // parent ever refuses an onChange (focus would then sit on an unchecked
  // option and the next arrow would move from the wrong place), and it is
  // worth re-reading if this control ever carries more than two options.
  const moveTo = (i: number) => {
    const next = (i + options.length) % options.length;
    onChange(options[next].value);
    // Focus follows selection, so the roving tab stop and the focus ring stay
    // on the same option. The element is focused directly rather than left to
    // the re-render, because the new tabIndex alone does not move focus.
    refs.current[next]?.focus();
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    // preventDefault on the arrows: inside a radiogroup they belong to the
    // group, and without this the page scrolls underneath the wizard.
    switch (e.key) {
      case 'ArrowRight': case 'ArrowDown': e.preventDefault(); moveTo(tabbable + 1); break;
      case 'ArrowLeft': case 'ArrowUp': e.preventDefault(); moveTo(tabbable - 1); break;
      case 'Home': e.preventDefault(); moveTo(0); break;
      case 'End': e.preventDefault(); moveTo(options.length - 1); break;
    }
  };

  return (
    <div style={segGroup} role="radiogroup" aria-label={label} onKeyDown={onKeyDown}>
      {options.map((o, i) => {
        const on = i === selected;
        return (
          <button
            key={o.value}
            ref={el => { refs.current[i] = el; }}
            type="button"
            role="radio"
            aria-checked={on}
            tabIndex={i === tabbable ? 0 : -1}
            data-testid={o.testid}
            style={segment(on, o.tone)}
            onClick={() => onChange(o.value)}
          >
            <span style={segDot(on, o.tone)} />
            <span>
              {o.title}
              <span style={segSub(on, o.tone)}>{o.sub}</span>
            </span>
          </button>
        );
      })}
    </div>
  );
}
