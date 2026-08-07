// The mounts picker is the lens's scope control. It used to be a labelled
// dropdown in the left panel while the top bar rendered `lens.reads.length`
// mounts — a readout of the TOTAL, never the selection. Two places showed the
// same fact and only one of them was true, so the readout became the control.
//
// The menu is an OVERLAY. Rendered in flow it grew its container and shoved the
// fact list down by its own height, so opening it moved the rows the reader was
// aiming at — and toggling a mount re-laid-out the list underneath. That is a
// property of the menu, not of where it lives, so it survives the move.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MountsPicker } from './MountsPicker';
import type { Lens } from './api';

const lens: Lens = {
  name: 'all', write: 'test',
  reads: [{ repo: 'core' }, { repo: 'docs' }, { repo: 'infra' }],
};

function mount(selection: string[] | null = null, dispatch = vi.fn()) {
  render(<MountsPicker lens={lens} selection={selection} dispatch={dispatch} />);
  return dispatch;
}
function openMenu(selection: string[] | null = null, dispatch = vi.fn()) {
  mount(selection, dispatch);
  fireEvent.click(screen.getByTestId('mounts-picker'));
  return screen.getByTestId('mounts-menu');
}
const label = () => screen.getByTestId('mounts-label').textContent!;

describe('MountsPicker', () => {
  describe('the label', () => {
    it('shows the bare total at rest', () => {
      mount(null);
      expect(label()).toBe('3');
    });

    it('shows a ratio once the scope is narrowed', () => {
      mount(['core']);
      expect(label()).toBe('1/3');
    });

    it('shows a ratio for a selection of none', () => {
      // [] is "none selected", which is a real state the toggles can reach and
      // the one most in need of an explanation for an empty list.
      mount([]);
      expect(label()).toBe('0/3');
    });

    it('reports a full explicit selection as rest, not as a ratio', () => {
      // Belt and braces: the reducer collapses a complete set to null, but if
      // one ever arrives explicitly it means "all mounts" and must not read as
      // a filter — 3/3 would claim a narrowing that is not there.
      mount(['core', 'docs', 'infra']);
      expect(label()).toBe('3');
    });

    it('looks different when narrowed — a filtered scope must not read as resting', () => {
      // The label silently explains why the list is short. If it looked the
      // same either way, a one-mount view would be indistinguishable from the
      // whole union and the reader would have no reason to suspect a filter.
      const { unmount } = render(<MountsPicker lens={lens} selection={null} dispatch={vi.fn()} />);
      const rest = screen.getByTestId('mounts-picker').style.color;
      unmount();
      render(<MountsPicker lens={lens} selection={['core']} dispatch={vi.fn()} />);
      expect(screen.getByTestId('mounts-picker').style.color).not.toBe(rest);
    });
  });

  describe('the menu', () => {
    it('floats over the layout instead of pushing it down', () => {
      const menu = openMenu();
      expect(menu.style.position).toBe('absolute');
      expect(Number(menu.style.zIndex)).toBeGreaterThan(0);
    });

    it('anchors to the trigger, not to whatever ancestor happens to be positioned', () => {
      const menu = openMenu();
      expect(menu.style.top).toBe('100%');
      expect(menu.parentElement!.style.position).toBe('relative');
    });

    it('scrolls rather than running off the screen when a lens has many mounts', () => {
      const menu = openMenu();
      expect(menu.style.maxHeight).not.toBe('');
      expect(menu.style.overflowY).toBe('auto');
    });

    it('is closed until asked for', () => {
      mount();
      expect(screen.queryByTestId('mounts-menu')).toBeNull();
    });
  });

  describe('toggling', () => {
    it('takes a mount off the full selection', () => {
      const dispatch = vi.fn();
      openMenu(null, dispatch);
      fireEvent.click(screen.getByTestId('mounts-menu').querySelector('[data-repo="docs"]')!);
      expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: ['core', 'infra'] });
    });

    it('collapses a re-completed selection back to all mounts', () => {
      // null, not the full array — that is what drops the repos param so the
      // server fans out instead of being handed every mount by name.
      const dispatch = vi.fn();
      openMenu(['core', 'infra'], dispatch);
      fireEvent.click(screen.getByTestId('mounts-menu').querySelector('[data-repo="docs"]')!);
      expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: null });
    });

    it('preserves server order regardless of the order mounts were picked', () => {
      const dispatch = vi.fn();
      openMenu(['infra'], dispatch);
      fireEvent.click(screen.getByTestId('mounts-menu').querySelector('[data-repo="core"]')!);
      expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: ['core', 'infra'] });
    });
  });

  describe('all / none', () => {
    it('offers a way back to every mount without clicking each one', () => {
      const dispatch = vi.fn();
      openMenu(['core'], dispatch);
      fireEvent.click(screen.getByTestId('mounts-all'));
      expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: null });
    });

    it('offers a way to clear the scope in one click', () => {
      const dispatch = vi.fn();
      openMenu(null, dispatch);
      fireEvent.click(screen.getByTestId('mounts-none'));
      expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: [] });
    });
  });

  // This control lives inside the top bar, which is the desktop build's window
  // drag handle: TopBar starts a native drag on mousedown anywhere on the bar
  // unless the target has a [data-nodrag] ANCESTOR. The menu is the trigger's
  // sibling, so tagging the trigger alone left every mount option inside the
  // drag region — mousedown posted wails:drag and the native drag loop ate the
  // click, so on the desktop app the options could not be toggled at all. The
  // tag has to sit on the root, which is the only node that contains both.
  describe('the desktop drag region', () => {
    it('keeps the whole control — menu included — out of it', () => {
      const menu = openMenu(null);
      expect(menu.closest('[data-nodrag]')).not.toBeNull();
      expect(screen.getByTestId('mounts-picker').closest('[data-nodrag]')).not.toBeNull();
      // Every option, not just the container: the drag check runs from the
      // actual mousedown target.
      for (const opt of screen.getAllByTestId('mount-option')) {
        expect(opt.closest('[data-nodrag]')).not.toBeNull();
      }
    });
  });
});
