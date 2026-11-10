// authState is whether the server has asked this browser to sign in (unified
// auth, P3). An API answer of 401 sets it (api.ts apiFetch, and a stream that
// fails with 401); App renders the login page while it is set.
//
// Before the unified-auth flip (P4) the server never answers the UI with 401,
// so the login page is unreachable: this is plumbing, built dark.

type Listener = () => void;

let required = false;
const listeners = new Set<Listener>();

function notify(): void {
  for (const l of [...listeners]) l();
}

export const authState = {
  /** Switch to the login page. Idempotent: a second call while set does nothing. */
  requireLogin(): void {
    if (required) return;
    required = true;
    notify();
  },
  /** The browser holds a session again. */
  loggedIn(): void {
    if (!required) return;
    required = false;
    notify();
  },
  isLoginRequired(): boolean {
    return required;
  },
  subscribe(l: Listener): () => void {
    listeners.add(l);
    return () => { listeners.delete(l); };
  },
  /** Tests only: forget everything. */
  reset(): void {
    required = false;
    listeners.clear();
  },
};

/**
 * noteStatus is what every API response passes through: a 401 asks for login,
 * once (requireLogin is not called again while login is already required).
 */
export function noteStatus(status: number): void {
  if (status === 401 && !authState.isLoginRequired()) authState.requireLogin();
}
