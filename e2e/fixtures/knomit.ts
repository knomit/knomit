/**
 * Playwright test fixtures for knomit e2e tests.
 *
 * - sharedBaseURL: reads baseURL from .e2e-state.json (read-only tests)
 * - freshKnomit: spins up a fresh knomit instance per test (mutation tests)
 * - page: signed in to the shared instance as the owner (a one-time login
 *   link minted over the local listener) before the test's first goto
 * - request: an API request context carrying the shared instance's
 *   operator PAT
 *
 * The credentials are the unified-auth bootstrap (P3): the server ignores
 * them until the flip (P4), after which nothing works without them.
 */

import { spawn, type ChildProcess } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { test as base, expect, type APIRequestContext } from '@playwright/test';
import { bearer, mintPAT, ownerLoginURL } from '../helpers/auth.js';
import getPort from 'get-port';
import { onnxEnv, requireBuild } from '../helpers/binary.js';
import { knomitModelsDir } from '../helpers/appdirs.js';
import { createRepo, discoverAgentBranch } from './seed.js';

const STATE_FILE = resolve(import.meta.dirname, '..', '.e2e-state.json');

// ── State types ─────────────────────────────────────────────────

interface E2EState {
  pid: number;
  port: number;
  home: string;
  baseURL: string;
  pat?: string;
}

interface FreshKnomit {
  baseURL: string;
  branch: string;
  /** Carries an operator PAT minted for this instance. */
  api: APIRequestContext;
  /** Its home, for minting more credentials (helpers/auth.ts). */
  home: string;
}

// ── Helpers ─────────────────────────────────────────────────────

function readState(): E2EState {
  if (!existsSync(STATE_FILE)) {
    throw new Error(
      `State file not found at ${STATE_FILE}. Did global-setup run?`,
    );
  }
  return JSON.parse(readFileSync(STATE_FILE, 'utf-8'));
}

async function waitForHealthy(baseURL: string, timeoutMs = 60_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      // A PUBLIC route: after the unified-auth flip /api/v1/repos needs a
      // credential, /api/v1/version never does.
      const res = await fetch(`${baseURL}/api/v1/version`);
      if (res.ok) return;
    } catch {
      // not ready yet
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`knomit at ${baseURL} did not become healthy within ${timeoutMs}ms`);
}

// ── Fixtures ────────────────────────────────────────────────────

export const test = base.extend<{
  sharedBaseURL: string;
  sharedBranch: string;
  freshKnomit: FreshKnomit;
}>({
  /**
   * Base URL of the shared knomit instance started by global-setup.
   * Use for read-only tests that don't mutate state.
   */
  sharedBaseURL: async ({}, use) => {
    const state = readState();
    await use(state.baseURL);
  },

  /**
   * The encoded agent branch name for the shared knomit instance.
   * Discovered once per test via GET /api/v1/repos/knomit/branches.
   */
  sharedBranch: async ({ sharedBaseURL }, use) => {
    const pat = readState().pat;
    const branch = await discoverAgentBranch(sharedBaseURL, undefined, pat ? bearer(pat) : {});
    await use(branch);
  },

  /** Playwright's request context, with the shared instance's operator PAT. */
  request: async ({ playwright, baseURL }, use) => {
    const pat = readState().pat;
    const ctx = await playwright.request.newContext({ baseURL, extraHTTPHeaders: pat ? bearer(pat) : {} });
    await use(ctx);
    await ctx.dispose();
  },

  /** The page, signed in to the shared instance as the owner first. */
  page: async ({ page }, use) => {
    const state = readState();
    await page.goto(await ownerLoginURL(state.home), { waitUntil: 'domcontentloaded' });
    await use(page);
  },

  /**
   * A fresh knomit instance created for a single test.
   * Use for tests that need isolated, mutable state.
   */
  freshKnomit: async ({ playwright }, use) => {
    const binaryPath = requireBuild();

    const port = await getPort();
    const knomitHome = mkdtempSync(join(tmpdir(), 'knomit-e2e-fresh-'));

    // Copy ONNX model cache if available
    const userModelsDir = knomitModelsDir();
    const targetModelsDir = join(knomitHome, 'models');
    if (existsSync(userModelsDir)) {
      cpSync(userModelsDir, targetModelsDir, { recursive: true });
    }

    const env: Record<string, string> = {
      ...process.env as Record<string, string>,
      KNOMIT_HOME: knomitHome,
      KNOMIT_PORT: String(port),
      ...onnxEnv(),
    };

    const child: ChildProcess = spawn(binaryPath, ['serve'], {
      env,
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    child.stdout?.on('data', (data: Buffer) => {
      for (const line of data.toString().split('\n').filter(Boolean)) {
        console.log(`[fresh-knomit:${port}] ${line}`);
      }
    });
    child.stderr?.on('data', (data: Buffer) => {
      for (const line of data.toString().split('\n').filter(Boolean)) {
        console.error(`[fresh-knomit:${port}] ${line}`);
      }
    });

    child.unref();
    const pid = child.pid;
    if (!pid) throw new Error('Failed to start fresh knomit process');

    const baseURL = `http://localhost:${port}`;
    let api: APIRequestContext | undefined;

    // From here on the server and home exist: every exit path, including a
    // failed health wait or repo create, must kill and remove them.
    try {
      await waitForHealthy(baseURL);
      const headers = bearer(await mintPAT(knomitHome, 'operator'));

      // A fresh server has no repos, so make the one the tests write to.
      await createRepo(baseURL, undefined, headers);
      const branch = await discoverAgentBranch(baseURL, undefined, headers);
      api = await playwright.request.newContext({ baseURL, extraHTTPHeaders: headers });
      await use({ baseURL, branch, api, home: knomitHome });
    } finally {
      await api?.dispose();

      // Kill process
      try { process.kill(-pid, 'SIGTERM'); } catch {
        try { process.kill(pid, 'SIGTERM'); } catch { /* already exited */ }
      }

      // Remove temp dir
      rmSync(knomitHome, { recursive: true, force: true });
    }
  },
});

export { expect };
