// Races: a stale page after an option change, and aborts on unmount and on an option change.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { MaterializeView } from './MaterializeView';
import { useMaterializeStream } from './useMaterializeStream';
import { renderHook } from '@testing-library/react';
import { DEFAULT_OPTIONS } from './materializeApi';
import type { MaterializeApi, MatNode, MatOptions, MatPage } from './materializeApi';

class MockIO {
  static all: MockIO[] = [];
  live = false;
  cb: IntersectionObserverCallback;
  constructor(cb: IntersectionObserverCallback) { this.cb = cb; MockIO.all.push(this); }
  observe() { this.live = true; }
  unobserve() {}
  disconnect() { this.live = false; }
  takeRecords() { return []; }
}
const realIO = globalThis.IntersectionObserver;
beforeEach(() => { MockIO.all = []; globalThis.IntersectionObserver = MockIO as unknown as typeof IntersectionObserver; });
afterEach(() => { globalThis.IntersectionObserver = realIO; vi.restoreAllMocks(); });
function nearBottom() {
  act(() => { for (const o of MockIO.all.filter(x => x.live)) o.cb([{ isIntersecting: true } as IntersectionObserverEntry], o as unknown as IntersectionObserver); });
}
const titles = () => screen.queryAllByTestId('mat-section').map(e => e.querySelector('.k-mat-h')?.textContent);

const TARGET = { repo: 'kb', branch: 'main', path: 'kb/x/abc12345.md' };
function mk(tag: string, from: number, to: number, cursor: string | null): MatPage {
  return {
    root: 'r', anchor: 'abc1234', order: 'refs', unpinned: false, cross_kb: false, depth: 4, max_nodes: 200,
    total: 9, truncated: { depth: 0, nodes: 0 }, n_from: from, n_to: to, cursor, has_more: cursor !== null,
    facts: Array.from({ length: to - from + 1 }, (_, i) => ({
      n: from + i, path: `kb/${tag}/${from + i}.md`, repo: 'r', commit: 'c', depth: 1, role: 'ref', title: `${tag}${from + i}`,
      type: 'observation', kind: 'epistemic', confidence: 0.9, body: 'b', anchor_id: `f-${tag}${from + i}`,
    }) as MatNode),
  };
}

function Harness({ api }: { api: MaterializeApi }) {
  const [options, setOptions] = useState<MatOptions>(DEFAULT_OPTIONS);
  return <MaterializeView target={TARGET} title="T" options={options} api={api} onOptionsChange={setOptions} onBack={() => {}} />;
}

describe('stream races', () => {
  it('a page of the OLD walk that lands after an option change is never appended (signal ignored by the server)', async () => {
    const held: Array<{ resolve: (p: MatPage) => void; depth: number; cursor: string | null }> = [];
    const api: MaterializeApi = {
      page: vi.fn((_t, o, cursor) => {
        if (o.depth === 4 && cursor === null) return Promise.resolve(mk('a', 1, 3, 'ca'));
        return new Promise<MatPage>(resolve => held.push({ resolve, depth: o.depth, cursor }));
      }),
      exportMarkdown: vi.fn(),
    };
    render(<Harness api={api} />);
    await waitFor(() => expect(titles()).toEqual(['a1', 'a2', 'a3']));
    nearBottom();                                     // old walk's page 2 now in flight
    await waitFor(() => expect(held).toHaveLength(1));
    fireEvent.click(screen.getByLabelText('Raise Max depth'));   // restart: new first call in flight
    await waitFor(() => expect(held).toHaveLength(2));
    await act(async () => { held[0].resolve(mk('a', 4, 6, 'ca')); });   // stale page lands first
    await act(async () => { held[1].resolve(mk('b', 1, 3, null)); });
    await waitFor(() => expect(titles()).toEqual(['b1', 'b2', 'b3']));
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(titles()).toEqual(['b1', 'b2', 'b3']);
  });

  it('the stale page landing AFTER the new page 1 is not appended either', async () => {
    const held: Array<{ resolve: (p: MatPage) => void }> = [];
    const api: MaterializeApi = {
      page: vi.fn((_t, o, cursor) => {
        if (o.depth === 4 && cursor === null) return Promise.resolve(mk('a', 1, 3, 'ca'));
        return new Promise<MatPage>(resolve => held.push({ resolve }));
      }),
      exportMarkdown: vi.fn(),
    };
    render(<Harness api={api} />);
    await waitFor(() => expect(titles()).toEqual(['a1', 'a2', 'a3']));
    nearBottom();
    await waitFor(() => expect(held).toHaveLength(1));
    fireEvent.click(screen.getByLabelText('Raise Max depth'));
    await waitFor(() => expect(held).toHaveLength(2));
    await act(async () => { held[1].resolve(mk('b', 1, 3, 'cb')); });
    await waitFor(() => expect(titles()).toEqual(['b1', 'b2', 'b3']));
    await act(async () => { held[0].resolve(mk('a', 4, 6, 'ca')); });
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(titles()).toEqual(['b1', 'b2', 'b3']);
  });

  it('unmount aborts the page in flight', async () => {
    const signals: AbortSignal[] = [];
    const api: MaterializeApi = {
      page: vi.fn((_t, _o, cursor, signal) => {
        signals.push(signal!);
        return cursor === null ? Promise.resolve(mk('a', 1, 3, 'ca')) : new Promise<MatPage>(() => {});
      }),
      exportMarkdown: vi.fn(),
    };
    const { unmount } = render(<Harness api={api} />);
    await waitFor(() => expect(titles()).toEqual(['a1', 'a2', 'a3']));
    nearBottom();
    await waitFor(() => expect(signals).toHaveLength(2));
    unmount();
    expect(signals[1].aborted).toBe(true);
  });

  it('an option change aborts the page in flight', async () => {
    const signals: AbortSignal[] = [];
    const api: MaterializeApi = {
      page: vi.fn((_t, o, cursor, signal) => {
        signals.push(signal!);
        return o.depth === 4 && cursor === null ? Promise.resolve(mk('a', 1, 3, 'ca')) : new Promise<MatPage>(() => {});
      }),
      exportMarkdown: vi.fn(),
    };
    render(<Harness api={api} />);
    await waitFor(() => expect(titles()).toEqual(['a1', 'a2', 'a3']));
    nearBottom();
    await waitFor(() => expect(signals).toHaveLength(2));
    fireEvent.click(screen.getByLabelText('Raise Max depth'));
    await waitFor(() => expect(signals).toHaveLength(3));
    expect(signals[1].aborted).toBe(true);
  });

  it('a stale page finishing late does not clear the newer load\'s busy flag (no duplicate request)', async () => {
    const held: Array<{ resolve: (p: MatPage) => void; depth: number; cursor: string | null }> = [];
    const api: MaterializeApi = {
      page: vi.fn((_t, o, cursor) => {
        if (o.depth === 4 && cursor === null) return Promise.resolve(mk('a', 1, 3, 'ca'));
        return new Promise<MatPage>(resolve => held.push({ resolve, depth: o.depth, cursor }));
      }),
      exportMarkdown: vi.fn(),
    };
    const { result, rerender } = renderHook(({ o }) => useMaterializeStream(api, TARGET, o), { initialProps: { o: DEFAULT_OPTIONS } });
    await waitFor(() => expect(result.current.sections).toHaveLength(3));
    act(() => result.current.loadMore());                       // old walk's page 2 in flight
    await waitFor(() => expect(held).toHaveLength(1));
    rerender({ o: { ...DEFAULT_OPTIONS, depth: 5 } });          // restart: new first call
    await waitFor(() => expect(held).toHaveLength(2));
    await act(async () => { held[1].resolve(mk('b', 1, 3, 'cb')); });
    await waitFor(() => expect(result.current.sections[0].title).toBe('b1'));
    act(() => result.current.loadMore());                       // new walk's page 2 in flight
    await waitFor(() => expect(held).toHaveLength(3));
    await act(async () => { held[0].resolve(mk('a', 4, 6, 'ca')); });   // the OLD page lands late
    act(() => result.current.retry());                          // must still see a load in flight (retry does not check tail)
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(held).toHaveLength(3);
  });
});
