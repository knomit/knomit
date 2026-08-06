// The sources menu is an OVERLAY. Rendered in flow it grew the left panel and
// shoved the fact list down by its own height, so opening it moved the rows the
// reader was aiming at — and toggling a mount re-laid-out the list underneath.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { SourcesDropdown } from './SourcesDropdown';
import type { Lens } from './api';

const lens: Lens = {
  name: 'all', write: 'test',
  reads: [{ repo: 'core' }, { repo: 'docs' }, { repo: 'infra' }],
};

function open(selection: string[] | null = null, dispatch = vi.fn()) {
  render(<SourcesDropdown lens={lens} selection={selection} dispatch={dispatch} />);
  fireEvent.click(screen.getByTestId('sources-dropdown'));
  return screen.getByTestId('sources-menu');
}

describe('SourcesDropdown', () => {
  it('floats the menu over the panel instead of pushing it down', () => {
    const menu = open();
    expect(menu.style.position).toBe('absolute');
    expect(Number(menu.style.zIndex)).toBeGreaterThan(0);
  });

  it('anchors the menu to the trigger, not to the panel', () => {
    // An absolute menu whose nearest positioned ancestor is the whole panel
    // would land somewhere else entirely — the trigger's own wrapper has to be
    // the containing block.
    const menu = open();
    expect(menu.style.top).toBe('100%');
    expect(menu.parentElement!.style.position).toBe('relative');
  });

  it('scrolls rather than running off the panel when a lens has many mounts', () => {
    const menu = open();
    expect(menu.style.maxHeight).not.toBe('');
    expect(menu.style.overflowY).toBe('auto');
  });

  it('still toggles a mount off the full selection', () => {
    const dispatch = vi.fn();
    render(<SourcesDropdown lens={lens} selection={null} dispatch={dispatch} />);
    fireEvent.click(screen.getByTestId('sources-dropdown'));
    fireEvent.click(screen.getByTestId('sources-menu').querySelector('[data-repo="docs"]')!);
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: ['core', 'infra'] });
  });

  it('collapses a re-completed selection back to "all mounts"', () => {
    const dispatch = vi.fn();
    render(<SourcesDropdown lens={lens} selection={['core', 'infra']} dispatch={dispatch} />);
    fireEvent.click(screen.getByTestId('sources-dropdown'));
    fireEvent.click(screen.getByTestId('sources-menu').querySelector('[data-repo="docs"]')!);
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: null });
  });
});
