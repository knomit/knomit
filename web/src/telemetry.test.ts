import { describe, it, expect, afterEach } from 'vitest';
import { pageview, track } from './telemetry';

describe('telemetry seam', () => {
  afterEach(() => { delete window.knomitTelemetry; });

  it('is a no-op when no host hook is defined', () => {
    delete window.knomitTelemetry;
    expect(() => {
      pageview('/library/foo');
      track('search_performed', { result_count: 3 });
    }).not.toThrow();
  });

  it('forwards events to a host-defined hook', () => {
    const calls: Array<[string, Record<string, string | number | boolean> | undefined]> = [];
    window.knomitTelemetry = (e, p) => { calls.push([e, p]); };

    pageview('/library/foo');
    track('fact_opened', { topic: 'architecture' });

    expect(calls[0][0]).toBe('page_view');
    expect(calls[0][1]?.page_path).toBe('/library/foo');
    expect(calls[1][0]).toBe('fact_opened');
    expect(calls[1][1]?.topic).toBe('architecture');
  });
});
