import { useEffect, useRef, useState } from 'react';
import { apiUrl } from './apiBase';

// LoginPage pairs this browser with the instance (unified auth P3, OPEN-12):
// it starts a pairing request, shows its short code, and long-polls until
// someone signed in approves it; the approval answers the wait with a session
// cookie, and then the app reloads as that session.
//
// App shows it only after an API request answered 401, which the server does
// not do before the unified-auth flip (P4).

type Phase =
  | { kind: 'starting' }
  | { kind: 'waiting'; code: string }
  | { kind: 'denied' }
  | { kind: 'error'; message: string };

interface PairStart { id: string; code: string; wait: string }

const RETRY_MS = 3000;

export function LoginPage({ onLoggedIn, minPollMs = 1000 }: { onLoggedIn: () => void; minPollMs?: number }) {
  const [phase, setPhase] = useState<Phase>({ kind: 'starting' });
  const done = useRef(onLoggedIn);
  done.current = onLoggedIn;

  useEffect(() => {
    let stopped = false;
    const sleep = (ms: number) => new Promise(r => setTimeout(r, ms));

    async function start(): Promise<PairStart | null> {
      const r = await fetch(apiUrl('/auth/pair'), { method: 'POST' });
      if (!r.ok) throw new Error(`pairing could not start (HTTP ${r.status})`);
      return (await r.json()) as PairStart;
    }

    async function run() {
      while (!stopped) {
        let pair: PairStart | null;
        try {
          pair = await start();
        } catch (e) {
          if (stopped) return;
          setPhase({ kind: 'error', message: String((e as Error).message ?? e) });
          await sleep(RETRY_MS);
          continue;
        }
        if (stopped || !pair) return;
        setPhase({ kind: 'waiting', code: pair.code });
        for (;;) {
          let r: Response;
          try {
            const asked = Date.now();
            r = await fetch(apiUrl(pair.wait), { method: 'GET' });
            // The wait is a long-poll; one answered at once (a proxy that does
            // not hold it) must not turn into a tight loop.
            const left = minPollMs - (Date.now() - asked);
            if (r.status === 202 && left > 0) await sleep(left);
          } catch {
            if (stopped) return;
            await sleep(RETRY_MS);
            continue;
          }
          if (stopped) return;
          if (r.status === 200) { done.current(); return; }
          if (r.status === 202) continue; // still waiting: poll again
          if (r.status === 403) { setPhase({ kind: 'denied' }); return; }
          await sleep(minPollMs);
          break; // 404 / 410: the request is gone; start a new one
        }
      }
    }
    void run();
    return () => { stopped = true; };
  }, [minPollMs]);

  return (
    <div className="login-page" data-testid="login-page" style={{ maxWidth: '36em', margin: '4em auto', padding: '0 1em' }}>
      <h1>Sign in to knomit</h1>
      {phase.kind === 'starting' && <p>Asking the server for a pairing code…</p>}
      {phase.kind === 'waiting' && (
        <>
          <p>This browser is not signed in. Its pairing code is</p>
          <p data-testid="pair-code" style={{ fontSize: '2em', fontFamily: 'monospace', letterSpacing: '0.1em' }}>{phase.code}</p>
          <p data-testid="pair-help">
            Approve this browser from a signed-in browser or run <code>knomit auth approve {phase.code}</code>
          </p>
          <p>This page continues by itself once it is approved.</p>
        </>
      )}
      {phase.kind === 'denied' && (
        <p data-testid="pair-denied">The pairing request was denied. Reload this page to ask again.</p>
      )}
      {phase.kind === 'error' && <p data-testid="pair-error">{phase.message} — retrying…</p>}
    </div>
  );
}
