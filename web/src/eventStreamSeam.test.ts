import { describe, it, expect } from 'vitest';

describe('the event stream seam', () => {
  // Every stream the app opens must go through openEventStream, or that stream
  // stays on a plain EventSource and is dead on the desktop's Windows build.
  it('is the only place that constructs an EventSource', () => {
    const files = import.meta.glob(['./*.ts', './*.tsx'], {
      query: '?raw', import: 'default', eager: true,
    }) as Record<string, string>;
    const names = Object.keys(files);
    // Positive controls: the scan saw the source tree, and a known caller.
    expect(names.length).toBeGreaterThan(50);
    expect(files['./api.ts']).toContain('openEventStream(');
    expect(files['./App.tsx']).toContain('openEventStream(');
    expect(files['./logStore.ts']).toContain('openEventStream(');
    const offenders = names.filter((n) =>
      !/\.test\.tsx?$/.test(n) && n !== './eventStream.ts' && n !== './testEventSource.ts'
      && /new EventSource\(/.test(files[n]));
    expect(offenders).toEqual([]);
  });
});
