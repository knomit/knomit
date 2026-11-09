import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { StrictMode } from 'react';
import { ChatTitleEdit } from './ChatTitleEdit';

// The editor on its own, with a parent that keeps it mounted after onDone, so
// nothing but the editor's own guard stands between one save and two.
describe('ChatTitleEdit', () => {
  // SABOTAGE: drop the done guard in finish -> red (onDone twice).
  it('Enter then the blur that follows it calls onDone exactly once, with the name', () => {
    const onDone = vi.fn();
    render(<ChatTitleEdit initial="old name" testId="edit" onDone={onDone} />);
    const input = screen.getByTestId('edit');
    fireEvent.change(input, { target: { value: '  New   name ' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    fireEvent.blur(input);
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(onDone).toHaveBeenCalledWith('New name');
  });

  // main.tsx renders under StrictMode, which in dev mounts, cleans up and mounts
  // again: the unmount cleanup must not leave the editor deaf to its own keys.
  // SABOTAGE: drop the done reset at the start of the effect -> red (0 calls).
  it('under StrictMode, Enter saves once, and Enter then blur saves once', () => {
    const onDone = vi.fn();
    const { unmount } = render(<StrictMode><ChatTitleEdit initial="old name" testId="edit" onDone={onDone} /></StrictMode>);
    let input = screen.getByTestId('edit');
    fireEvent.change(input, { target: { value: 'Strict one' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(onDone).toHaveBeenCalledWith('Strict one');
    unmount();

    const again = vi.fn();
    render(<StrictMode><ChatTitleEdit initial="old name" testId="edit" onDone={again} /></StrictMode>);
    input = screen.getByTestId('edit');
    fireEvent.change(input, { target: { value: 'Strict two' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    fireEvent.blur(input);
    expect(again).toHaveBeenCalledTimes(1);
    expect(again).toHaveBeenCalledWith('Strict two');
  });

  // SABOTAGE: drop the isComposing check -> red.
  it('an Enter that ends an IME composition is not a save', () => {
    const onDone = vi.fn();
    render(<ChatTitleEdit initial="old name" testId="edit" onDone={onDone} />);
    const input = screen.getByTestId('edit');
    fireEvent.change(input, { target: { value: '名前' } });
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
    expect(onDone).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onDone).toHaveBeenCalledWith('名前');
  });
});
