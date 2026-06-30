// Vendor-neutral, opt-in telemetry seam. The app emits anonymous usage signals
// through an OPTIONAL global hook that is UNDEFINED by default. A stock build
// therefore sends nothing, makes no network calls, and contains NO analytics
// vendor code (no GA, no gtag, no "google" string). A host that wants usage
// data — e.g. the public demo's reverse proxy — may define window.knomitTelemetry
// to receive these signals and forward them wherever it likes. This file
// intentionally references no analytics provider.

declare global {
  interface Window {
    knomitTelemetry?: (
      event: string,
      params?: Record<string, string | number | boolean>,
    ) => void;
  }
}

export function pageview(path: string): void {
  window.knomitTelemetry?.('page_view', { page_path: path });
}

export function track(
  event: string,
  params: Record<string, string | number | boolean> = {},
): void {
  window.knomitTelemetry?.(event, params);
}
