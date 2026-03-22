/**
 * Playwright test fixtures for knomit e2e tests.
 *
 * - sharedBaseURL: reads baseURL from .e2e-state.json (read-only tests)
 * - freshKnomit: spins up a fresh knomit instance per test (mutation tests)
 */

import { spawn, type ChildProcess } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { test as base, expect, type APIRequestContext } from '@playwright/test';
import getPort from 'get-port';

const PROJECT_ROOT = resolve(import.meta.dirname, '..', '..');
const STATE_FILE = resolve(import.meta.dirname, '..', '.e2e-state.json');

// ── State types ─────────────────────────────────────────────────

interface E2EState {
  pid: number;
  port: number;
  home: string;
  baseURL: string;
}

interface FreshKnomit {
  baseURL: string;
  api: APIRequestContext;
}

// ── Helpers ─────────────────────────────────────────────────────

function getOnnxLibName(): string {
  switch (process.platform) {
    case 'darwin':
      return 'libonnxruntime.dylib';
    case 'win32':
      return 'onnxruntime.dll';
    default:
      return 'libonnxruntime.so';
  }
}

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
      const res = await fetch(`${baseURL}/api/v1/repos`);
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
   * A fresh knomit instance created for a single test.
   * Use for tests that need isolated, mutable state.
   */
  freshKnomit: async ({ playwright }, use) => {
    const binaryPath = join(PROJECT_ROOT, 'dist', 'knomit');
    if (!existsSync(binaryPath)) {
      throw new Error(`Binary not found at ${binaryPath}. Run 'make dist' first.`);
    }

    const port = await getPort();
    const knomitHome = mkdtempSync(join(tmpdir(), 'knomit-e2e-fresh-'));

    // Copy ONNX model cache if available
    const userModelsDir = join(process.env.HOME || '~', '.knomit', 'models');
    const targetModelsDir = join(knomitHome, 'models');
    if (existsSync(userModelsDir)) {
      cpSync(userModelsDir, targetModelsDir, { recursive: true });
    }

    const onnxLib = join(PROJECT_ROOT, 'dist', 'lib', getOnnxLibName());
    const env: Record<string, string> = {
      ...process.env as Record<string, string>,
      KNOMIT_HOME: knomitHome,
      KNOMIT_PORT: String(port),
      ONNXRUNTIME_SHARED_LIBRARY: onnxLib,
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
    await waitForHealthy(baseURL);

    const api = await playwright.request.newContext({ baseURL });

    try {
      await use({ baseURL, api });
    } finally {
      await api.dispose();

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
