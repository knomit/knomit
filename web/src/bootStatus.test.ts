import { describe, it, expect, afterEach } from 'vitest';
import { pollBootStatus, isDesktopBooting, adoptAPIBase } from './bootStatus';
import type { BootStatus } from './bootStatus';

type W = Window & { __KNOMIT_BOOTING__?: boolean; __KNOMIT_API_BASE__?: string };

afterEach(() => {
  delete (window as W).__KNOMIT_BOOTING__;
  delete (window as W).__KNOMIT_API_BASE__;
});

const noSleep = async () => {};

describe('isDesktopBooting', () => {
  it('is false in the browser, where /config.js sets no such flag', () => {
    expect(isDesktopBooting()).toBe(false);
  });

  it('is true only for the literal flag the desktop writes', () => {
    (window as W).__KNOMIT_BOOTING__ = true;
    expect(isDesktopBooting()).toBe(true);
  });
});

describe('adoptAPIBase', () => {
  // api.ts reads window.__KNOMIT_API_BASE__ on every call, so this assignment
  // is the whole handover — no reload, no second /config.js fetch.
  it('installs the base and clears the booting flag together', () => {
    (window as W).__KNOMIT_BOOTING__ = true;
    adoptAPIBase('http://127.0.0.1:54321');
    expect((window as W).__KNOMIT_API_BASE__).toBe('http://127.0.0.1:54321');
    expect(isDesktopBooting()).toBe(false);
  });

  // An empty base would resolve every API call against the webview origin,
  // where the SPA fallback answers 200 with index.html. Refusing to adopt it
  // keeps the gate closed instead of opening it onto nonsense.
  it('refuses an empty base rather than pointing the app at the webview origin', () => {
    (window as W).__KNOMIT_BOOTING__ = true;
    adoptAPIBase('');
    expect((window as W).__KNOMIT_API_BASE__).toBeUndefined();
    expect(isDesktopBooting()).toBe(true);
  });
});

describe('pollBootStatus', () => {
  it('reports every phase and stops once ready', async () => {
    const queue: BootStatus[] = [
      { ready: false, phase: 'installing-tools' },
      { ready: false, phase: 'downloading-models' },
      { ready: true, phase: 'ready', api_base: 'http://127.0.0.1:54321' },
      { ready: true, phase: 'ready' }, // must never be reached
    ];
    const seen: string[] = [];

    await pollBootStatus({
      onStatus: (s) => seen.push(s.phase),
      shouldStop: () => false,
      fetchStatus: async () => queue.shift()!,
      sleep: noSleep,
    });

    expect(seen).toEqual(['installing-tools', 'downloading-models', 'ready']);
    expect(queue).toHaveLength(1); // stopped at ready, did not poll again
  });

  it('stops on a failed boot and hands the reason on', async () => {
    const seen: BootStatus[] = [];
    await pollBootStatus({
      onStatus: (s) => seen.push(s),
      shouldStop: () => false,
      fetchStatus: async () => ({ ready: false, phase: 'failed', error: 'embedder init failed' }),
      sleep: noSleep,
    });

    expect(seen).toHaveLength(1);
    expect(seen[0].error).toBe('embedder init failed');
  });

  // A dropped request during startup says nothing about the SERVER — the
  // webview is still settling and the desktop process may not have its handler
  // wired for another moment. Treating it as a failed boot would put an error
  // screen in front of a boot that is proceeding perfectly well.
  it('rides out a failed request instead of calling the boot dead', async () => {
    let calls = 0;
    const seen: string[] = [];

    await pollBootStatus({
      onStatus: (s) => seen.push(s.phase),
      shouldStop: () => false,
      fetchStatus: async () => {
        calls += 1;
        if (calls <= 2) throw new Error('fetch failed');
        return { ready: true, phase: 'ready', api_base: 'http://x' };
      },
      sleep: noSleep,
    });

    expect(calls).toBe(3);
    expect(seen).toEqual(['ready']); // the two failures reported nothing
  });

  it('gives up when the caller unmounts', async () => {
    let stop = false;
    let polls = 0;

    await pollBootStatus({
      onStatus: () => { stop = true; },
      shouldStop: () => stop,
      fetchStatus: async () => { polls += 1; return { ready: false, phase: 'downloading-models' }; },
      sleep: noSleep,
    });

    expect(polls).toBe(1);
  });
});
