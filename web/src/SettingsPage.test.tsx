import { describe, it, expect } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';

// SettingsPage is the Settings facet's body: an index column (the target's
// kind as its heading, one item per block) beside the scrolling blocks. The
// index is what the 178px "On this page" rail became; it now sits on the left
// at the Library's width.

const SECTIONS: Section[] = [
  { id: 'description', title: 'Description', body: <p>about</p> },
  { id: 'remote', title: 'Remote', body: <p>origin</p>, problem: true },
  { id: 'license', title: 'Licence', body: <p>MIT</p> },
  { id: 'index', title: 'Index', body: <p>rebuild</p> },
  { id: 'danger', title: 'Danger zone', danger: true, body: <p>archive</p> },
];

describe('SettingsPage', () => {
  it('heads the index with the target kind, never "On this page"', () => {
    const { unmount } = render(<SettingsPage kind="repo" sections={SECTIONS} />);
    expect(screen.getByTestId('settings-index-heading').textContent).toBe('Repository');
    expect(screen.queryByText(/On this page/i)).toBeNull();
    unmount();

    render(<SettingsPage kind="lens" sections={SECTIONS} />);
    expect(screen.getByTestId('settings-index-heading').textContent).toBe('Lens');
    expect(screen.queryByText(/On this page/i)).toBeNull();
  });

  it('marks an index item current when it is clicked', () => {
    render(<SettingsPage kind="repo" sections={SECTIONS} />);
    fireEvent.click(screen.getByTestId('toc-license'));
    expect(screen.getByTestId('toc-license')).toHaveAttribute('aria-current', 'true');
    expect(screen.getByTestId('toc-description')).not.toHaveAttribute('aria-current');
  });

  it('puts a problem dot on the item whose block has a problem, and on no other', () => {
    render(<SettingsPage kind="repo" sections={SECTIONS} />);
    expect(screen.getByTestId('toc-remote-problem')).toBeInTheDocument();
    expect(screen.queryByTestId('toc-license-problem')).toBeNull();
    expect(screen.queryByTestId('toc-description-problem')).toBeNull();
  });

  it('colours the danger item #c77', () => {
    render(<SettingsPage kind="repo" sections={SECTIONS} />);
    expect(screen.getByTestId('toc-danger').style.color).toBe('rgb(204, 119, 119)');
    // Control: an ordinary, inactive item is the index's grey.
    expect(screen.getByTestId('toc-license').style.color).toBe('rgb(153, 153, 153)');
  });

  // The page owns its scroll container now (no walk up to a scroll parent), so
  // the scroll listener is on `settings-scroll`.
  //
  // NOTE jsdom has no layout, so every rect is zero and `compute` reads that as
  // "scrolled to the end" — which is why the LAST entry is the one that ends up
  // current. What this pins is that a listener is attached at all, on a page
  // that does not overflow yet.
  it('tracks scrolling on a page that does not overflow yet', async () => {
    render(<SettingsPage kind="repo" sections={SECTIONS} />);
    fireEvent.scroll(screen.getByTestId('settings-scroll'));
    await waitFor(() => expect(screen.getByTestId('toc-danger')).toHaveAttribute('aria-current', 'true'));
  });

  // The pin survives the scroll its own smooth animation fires. Listening for
  // `scroll` to release it would have cancelled it a frame after it was set.
  it('keeps an index entry selected after you click it', () => {
    render(<SettingsPage kind="repo" sections={SECTIONS} />);
    fireEvent.click(screen.getByTestId('toc-index'));
    expect(screen.getByTestId('toc-index')).toHaveAttribute('aria-current', 'true');
    expect(screen.getByTestId('toc-danger')).not.toHaveAttribute('aria-current');
    fireEvent.scroll(screen.getByTestId('settings-scroll'));
    expect(screen.getByTestId('toc-index')).toHaveAttribute('aria-current', 'true');
  });

  it('releases the selection when you scroll under your own steam', async () => {
    render(<SettingsPage kind="repo" sections={SECTIONS} />);
    // NOT the last section. jsdom has no layout (every scroll metric is 0), so
    // the scroll-spy always decides "scrolled to the end" and spies the LAST
    // block. Releasing a pin on that block would leave it current by the
    // spy's own account (knomit#270), so pin a middle one.
    fireEvent.click(screen.getByTestId('toc-index'));
    expect(screen.getByTestId('toc-index')).toHaveAttribute('aria-current', 'true');
    // Let the spy's first frame land, so the release is judged against its
    // settled answer every run, not only when the runner is slow.
    await act(async () => { await new Promise(r => requestAnimationFrame(() => r(undefined))); });
    expect(screen.getByTestId('toc-index')).toHaveAttribute('aria-current', 'true');

    fireEvent.wheel(window);
    await waitFor(() => expect(screen.getByTestId('toc-index')).not.toHaveAttribute('aria-current'));
  });

  it('lands on the focused block, and reports it consumed', async () => {
    const seen: string[] = [];
    const proto = HTMLElement.prototype as unknown as { scrollIntoView?: () => void };
    const orig = proto.scrollIntoView;
    proto.scrollIntoView = function (this: HTMLElement) { seen.push(this.id); };
    try {
      let consumed = 0;
      render(<SettingsPage kind="repo" sections={SECTIONS} focus="remote" onFocused={() => { consumed++; }} />);
      await waitFor(() => expect(seen).toContain('remote'));
      expect(screen.getByTestId('toc-remote')).toHaveAttribute('aria-current', 'true');
      expect(consumed).toBe(1);
    } finally {
      proto.scrollIntoView = orig;
    }
  });
});
