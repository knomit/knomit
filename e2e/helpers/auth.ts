/**
 * Credentials for the e2e harness (unified auth P3). Both are minted over the
 * instance's LOCAL listener by the knomit binary itself, as the OS user that
 * runs the tests, which is the server's owner:
 *
 * - ownerLoginURL: a one-time link that signs a browser in as the owner;
 * - mintPAT: a short-lived personal access token for API request contexts.
 *
 * Before the unified-auth flip (P4) the server reads neither a session cookie
 * nor a token, so this only proves the plumbing; after it, it is how every
 * page and request authenticates.
 */
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { binaryPath } from './binary.js';

const run = promisify(execFile);

async function authJSON<T>(home: string, args: string[]): Promise<T> {
  const env = { ...(process.env as Record<string, string>), KNOMIT_HOME: home };
  const { stdout } = await run(binaryPath(), ['auth', ...args, '--json'], { env, timeout: 30_000 });
  return JSON.parse(stdout) as T;
}

/** A one-time (2 minute, single use) URL that signs a browser in as the owner. */
export async function ownerLoginURL(home: string): Promise<string> {
  const { url } = await authJSON<{ url: string }>(home, ['link']);
  return url;
}

/** A personal access token with the given role, valid for one hour. */
export async function mintPAT(home: string, role: 'contributor' | 'operator' | 'reader'): Promise<string> {
  const { secret } = await authJSON<{ secret: string }>(home, ['tokens', 'create', '--role', role, '--label', 'e2e', '--expires', '1h']);
  return secret;
}

/** The headers an API request context sends with a PAT. */
export function bearer(pat: string): Record<string, string> {
  return { Authorization: `Bearer ${pat}` };
}
