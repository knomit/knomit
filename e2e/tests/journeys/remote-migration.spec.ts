/**
 * E2E journey: origin-connect session lifecycle between two knomit instances.
 *
 * Starts TWO knomit instances ("remote" and "local") and checks that a
 * connect session can be created, tested and cancelled.
 *
 * The apply/commit half of the wizard (including conflict strategies) is
 * covered by Go tests, because this harness cannot seed a remote instance's
 * published main: see TestConnect_ConflictStrategyReachesTheReplay in
 * internal/edge/web/origin_wizard_conflict_strategy_test.go.
 */
import { type ChildProcess, spawn } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test as base, expect, type APIRequestContext } from '@playwright/test';
import getPort from 'get-port';
import { onnxEnv, requireBuild } from '../../helpers/binary.js';
import { createRepo, discoverAgentBranch } from '../../fixtures/seed.js';


interface KnomitInstance {
  baseURL: string;
  port: number;
  home: string;
  api: APIRequestContext;
  pid: number;
  branch: string;
}

async function startKnomit(label: string, playwright: any): Promise<{ instance: KnomitInstance; child: ChildProcess }> {
  const binaryPath = requireBuild();

  const port = await getPort();
  const home = mkdtempSync(join(tmpdir(), `knomit-e2e-${label}-`));

  const userModelsDir = join(process.env.HOME || '~', '.knomit', 'models');
  const targetModelsDir = join(home, 'models');
  if (existsSync(userModelsDir)) {
    cpSync(userModelsDir, targetModelsDir, { recursive: true });
  }

  const env: Record<string, string> = {
    ...process.env as Record<string, string>,
    KNOMIT_HOME: home,
    KNOMIT_PORT: String(port),
    ...onnxEnv(),
  };

  const child = spawn(binaryPath, ['serve'], {
    env,
    detached: true,
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  child.stdout?.on('data', (data: Buffer) => {
    for (const line of data.toString().split('\n').filter(Boolean)) {
      console.log(`[${label}:${port}] ${line}`);
    }
  });
  child.stderr?.on('data', (data: Buffer) => {
    for (const line of data.toString().split('\n').filter(Boolean)) {
      console.error(`[${label}:${port}] ${line}`);
    }
  });

  child.unref();
  const pid = child.pid;
  if (!pid) throw new Error(`Failed to start ${label} knomit process`);

  const baseURL = `http://localhost:${port}`;

  // Wait for healthy
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${baseURL}/api/v1/repos`);
      if (res.ok) break;
    } catch { /* not ready */ }
    await new Promise(r => setTimeout(r, 500));
  }

  // A setup failure from here on must not leak the server and its home: the
  // caller's fixture cleanup only covers instances that were returned.
  try {
    // A fresh server has no repos, so make the one the journey writes to.
    await createRepo(baseURL);
    const branch = await discoverAgentBranch(baseURL);
    const api = await playwright.request.newContext({ baseURL });
    return { instance: { baseURL, port, home, api, pid, branch }, child };
  } catch (err) {
    killInstance(pid);
    rmSync(home, { recursive: true, force: true });
    throw err;
  }
}

function killInstance(pid: number) {
  try { process.kill(-pid, 'SIGTERM'); } catch {
    try { process.kill(pid, 'SIGTERM'); } catch { /* already exited */ }
  }
}

// ── Helpers ─────────────────────────────────────────

function makeFact(title: string, domain: string, body: string): string {
  return `---
type: observation
domain: [${domain}]
confidence: 0.9
sources: 1
entities: [${domain}]
refs: []
---
# ${title}

${body}`;
}

async function seedFact(api: APIRequestContext, baseURL: string, branch: string, path: string, content: string) {
  const res = await api.put(
    `${baseURL}/api/v1/repos/knomit/branches/${branch}/facts/${path}`,
    { data: { content } },
  );
  expect(res.ok(), `seed ${path}: ${res.status()}`).toBeTruthy();
}

/** Parse SSE events from a response body string. */
function parseSSE(body: string): any[] {
  return body
    .split('\n')
    .filter(l => l.startsWith('data: '))
    .map(l => JSON.parse(l.slice(6)));
}

/** Read an SSE stream from a fetch Response. */
async function readSSE(res: Response): Promise<any[]> {
  const text = await res.text();
  return parseSSE(text);
}

// ── Test suite ──────────────────────────────────────

const test = base.extend<{ remoteMigration: { local: KnomitInstance; remote: KnomitInstance } }>({
  remoteMigration: async ({ playwright }, use) => {
    const { instance: remote } = await startKnomit('remote', playwright);
    let local: KnomitInstance | undefined;
    try {
      ({ instance: local } = await startKnomit('local', playwright));
      await use({ local, remote });
    } finally {
      await local?.api.dispose();
      await remote.api.dispose();
      if (local) killInstance(local.pid);
      killInstance(remote.pid);
      if (local) rmSync(local.home, { recursive: true, force: true });
      rmSync(remote.home, { recursive: true, force: true });
    }
  },
});

test.describe('Remote Migration Journey', () => {

  test('cancel mid-workflow cleans up session', async ({ remoteMigration }) => {
    const { local, remote } = remoteMigration;

    await seedFact(remote.api, remote.baseURL, remote.branch, 'kb/cancel-test/fact.md',
      makeFact('Cancel Test', 'testing', 'For cancel test.'));

    const remoteGitURL = `${remote.baseURL}/git/knomit`;

    // Create session and test
    const sessRes = await local.api.post(`${local.baseURL}/api/v1/repos/knomit/origin-sessions`, {
      data: { url: remoteGitURL, auth_method: '' },
    });
    const { session_id } = await sessRes.json();

    await readSSE(await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/test`));

    // Session exists
    const getRes = await local.api.get(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}`);
    expect(getRes.ok()).toBeTruthy();

    // Cancel (delete session)
    const delRes = await local.api.delete(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}`);
    expect(delRes.status()).toBe(204);

    // Session should be gone
    const checkRes = await local.api.get(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}`);
    expect(checkRes.status()).toBe(404);
  });
});
