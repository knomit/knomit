import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
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
