// Desktop-only: the boot gate.
//
// On the desktop the UI is served by the knomit-desktop process, which boots
// the knomit server BEHIND it. On a first launch that boot spends minutes
// fetching model artifacts, and until it finishes there is no API to call at
// all. /config.js says which world we are in (see configInjectingHandler in
// tools/desktop/app.go): a real __KNOMIT_API_BASE__ when the server is up, and
// __KNOMIT_BOOTING__ when it is not.
//
// While booting, THIS is the only endpoint that can answer anything — it is
// served by the desktop process itself, not by the server that does not exist
// yet. So the paths here are relative on purpose: they must resolve against the
// webview origin, never through apiUrl(), which prefixes a base that is by
// definition not set yet.
//
// Polling, not SSE. The stream ends at "ready", the payload is three small
// fields, and a held-open stream would be one more thing to unwind on the exact
// path where things are already fragile.

// BOOT_POLL_MS is the gap between polls. A second is well under the time any
// phase lasts and well over the cost of answering — the handler reads an atomic
// and encodes three fields.
export const BOOT_POLL_MS = 1000;

export const BOOT_STATUS_PATH = '/boot/status';

// BootStatus mirrors the Go bootStatus struct in tools/desktop/serverboot.go.
export interface BootStatus {
  ready: boolean;
  phase: string;
  api_base?: string;
  error?: string;
}

// isDesktopBooting reports whether this page loaded while the desktop server
// was still coming up.
//
// Read at CALL time rather than captured at module scope: /config.js runs as a
// blocking <script> before the bundle, so the flag is already there, but a test
// that sets it after import would otherwise see a stale answer.
export function isDesktopBooting(): boolean {
  return typeof window !== 'undefined' &&
    (window as Window & { __KNOMIT_BOOTING__?: boolean }).__KNOMIT_BOOTING__ === true;
}

// adoptAPIBase installs the base the desktop reported once its server is up.
//
// api.ts reads window.__KNOMIT_API_BASE__ on every call, so assigning it here is
// all it takes for the rest of the app to start working — no reload, and no
// second fetch of /config.js whose only purpose would be to set this string.
export function adoptAPIBase(base: string): void {
  if (typeof window === 'undefined' || !base) return;
  (window as Window & { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__ = base;
  delete (window as Window & { __KNOMIT_BOOTING__?: boolean }).__KNOMIT_BOOTING__;
}

export interface PollDeps {
  // Called with each status the desktop reports, including repeats.
  onStatus: (s: BootStatus) => void;
  shouldStop: () => boolean;
  // Injectable for tests; production uses fetch and setTimeout.
  fetchStatus?: () => Promise<BootStatus>;
  sleep?: (ms: number) => Promise<void>;
  intervalMs?: number;
}

const realFetchStatus = async (): Promise<BootStatus> => {
  const r = await fetch(BOOT_STATUS_PATH, { cache: 'no-store' });
  if (!r.ok) throw new Error(`boot status: HTTP ${r.status}`);
  return (await r.json()) as BootStatus;
};

const realSleep = (ms: number) => new Promise<void>((res) => setTimeout(res, ms));

// pollBootStatus reports the desktop's boot progress until it is ready, it
// fails, or shouldStop says to give up. It resolves rather than throwing: a
// terminal failure arrives through onStatus as a status with `error` set,
// because the boot screen renders that the same way it renders a phase.
//
// A failed POLL is not a failed boot and must not be reported as one — during
// startup the webview can lose a request for reasons that have nothing to do
// with the server — so a fetch error just waits and asks again.
export async function pollBootStatus(deps: PollDeps): Promise<void> {
  const fetchStatus = deps.fetchStatus ?? realFetchStatus;
  const sleep = deps.sleep ?? realSleep;
  const interval = deps.intervalMs ?? BOOT_POLL_MS;
  while (!deps.shouldStop()) {
    try {
      const s = await fetchStatus();
      if (deps.shouldStop()) return;
      deps.onStatus(s);
      if (s.ready || s.error) return;
    } catch {
      // Transient. Fall through to the sleep and try again.
    }
    await sleep(interval);
  }
}
