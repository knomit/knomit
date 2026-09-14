import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { SegmentedChoice, type SegmentedOption } from './SegmentedChoice';

// The control is a RADIOGROUP, and every test here pins a property that the
// group of aria-pressed buttons it replaced did not have. That is the point of
// the file: the pixels were already right, and a visual test would have passed
// on the old markup too.
const options: SegmentedOption<'a' | 'b'>[] = [
  { value: 'a', tone: 'remote', testid: 'opt-a', title: 'Alpha', sub: 'the first one' },
  { value: 'b', tone: 'neutral', testid: 'opt-b', title: 'Beta', sub: 'the second one' },
];

const setup = (value: 'a' | 'b' = 'a', onChange = vi.fn()) => {
  render(<SegmentedChoice label="Which one" value={value} onChange={onChange} options={options} />);
  return { onChange, a: screen.getByTestId('opt-a'), b: screen.getByTestId('opt-b') };
};

describe('SegmentedChoice', () => {
  // Mutual exclusion has to be ANNOUNCED, not merely implemented. role=radio
  // inside role=radiogroup is what makes a screen reader say "Alpha, radio
  // button, 1 of 2"; aria-pressed buttons announce two unrelated toggles and
  // never mention that picking one clears the other.
  it('is a radiogroup of radios, labelled', () => {
    const { a, b } = setup();
    const group = screen.getByRole('radiogroup', { name: 'Which one' });
    expect(group).toBeInTheDocument();
    expect(a).toHaveAttribute('role', 'radio');
    expect(b).toHaveAttribute('role', 'radio');
    expect(group).toContainElement(a);
    expect(group).toContainElement(b);
  });

  it('reflects the chosen value in aria-checked', () => {
    const { a, b } = setup('b');
    expect(b).toHaveAttribute('aria-checked', 'true');
    expect(a).toHaveAttribute('aria-checked', 'false');
  });

  // ONE tab stop for the group, landing on the checked option — the roving
  // tabindex. Two tab stops is what the old markup gave, which makes a
  // keyboard user tab through every option of every choice on the page.
  it('is a single tab stop, on the checked option', () => {
    const { a, b } = setup('b');
    expect(b).toHaveAttribute('tabindex', '0');
    expect(a).toHaveAttribute('tabindex', '-1');
  });

  // Arrow keys SELECT, which is native radio behaviour — not merely move
  // focus. Both halves are asserted: the caller is told, and focus follows so
  // the tab stop and the focus ring do not come apart.
  it('selects the next option on ArrowRight, and moves focus with it', () => {
    const { onChange, a, b } = setup('a');
    a.focus();
    fireEvent.keyDown(a, { key: 'ArrowRight' });
    expect(onChange).toHaveBeenCalledWith('b');
    expect(b).toHaveFocus();
  });

  it('wraps to the last option on ArrowLeft from the first', () => {
    const { onChange, a, b } = setup('a');
    a.focus();
    fireEvent.keyDown(a, { key: 'ArrowLeft' });
    expect(onChange).toHaveBeenCalledWith('b');
    expect(b).toHaveFocus();
  });

  it('treats ArrowDown and ArrowUp as the same axis', () => {
    const { onChange, b } = setup('b');
    b.focus();
    fireEvent.keyDown(b, { key: 'ArrowDown' });
    expect(onChange).toHaveBeenCalledWith('a');
  });

  it('still selects on click', () => {
    const { onChange, b } = setup('a');
    fireEvent.click(b);
    expect(onChange).toHaveBeenCalledWith('b');
  });

  // A value matching no option would otherwise give every option tabIndex -1,
  // and the control becomes unreachable by keyboard — no arrow key can help,
  // because Tab can never get inside the group to press one. Reachable from
  // outside this file: the component is generic over its option list, so any
  // caller whose value and options can disagree lands here.
  it('keeps a tab stop when the value matches no option', () => {
    const onChange = vi.fn();
    render(<SegmentedChoice label="Which one" value={'zz' as 'a'} onChange={onChange} options={options} />);
    expect(screen.getByTestId('opt-a')).toHaveAttribute('tabindex', '0');
    expect(screen.getByTestId('opt-a')).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByTestId('opt-b')).toHaveAttribute('aria-checked', 'false');
  });
});
