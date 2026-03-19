import { existsSync, readFileSync, rmSync, unlinkSync } from 'node:fs';
import { resolve } from 'node:path';

const STATE_FILE = resolve(import.meta.dirname, '.e2e-state.json');

interface E2EState {
  pid: number;
  port: number;
  home: string;
  baseURL: string;
}

function globalTeardown() {
  if (!existsSync(STATE_FILE)) {
    console.log('[e2e] No state file found, nothing to tear down');
    return;
  }

  const state: E2EState = JSON.parse(readFileSync(STATE_FILE, 'utf-8'));
  console.log(`[e2e] Tearing down (pid=${state.pid}, home=${state.home})`);

  // Kill the server process — try process group first, then direct
  try {
    process.kill(-state.pid, 'SIGTERM');
    console.log('[e2e] Killed process group');
  } catch {
    try {
      process.kill(state.pid, 'SIGTERM');
      console.log('[e2e] Killed process directly');
    } catch {
      console.log('[e2e] Process already exited');
    }
  }

  // Remove temp KNOMIT_HOME
  try {
    rmSync(state.home, { recursive: true, force: true });
    console.log('[e2e] Removed temp home');
  } catch (err) {
    console.warn(`[e2e] Failed to remove temp home: ${err}`);
  }

  // Remove state file
  try {
    unlinkSync(STATE_FILE);
  } catch {
    // ignore
  }
}

export default globalTeardown;
