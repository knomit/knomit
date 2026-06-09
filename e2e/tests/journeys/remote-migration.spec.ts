/**
 * E2E journey: migrate a local knomit to a remote repo.
 *
 * Starts TWO knomit instances:
 *   - "remote" instance: seeded with facts, serves as the git remote
 *   - "local" instance: has its own facts, connects to the remote
 *
 * Tests the full session workflow: create → test → preview → apply → commit,
 * then verifies the local instance has both its own and the remote's facts.
 */
import { type ChildProcess, spawn } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { test as base, expect, type APIRequestContext } from '@playwright/test';
import getPort from 'get-port';
import { discoverAgentBranch } from '../../fixtures/seed.js';

const PROJECT_ROOT = resolve(import.meta.dirname, '..', '..', '..');

interface KnomitInstance {
  baseURL: string;
  port: number;
  home: string;
  api: APIRequestContext;
  pid: number;
  branch: string;
}

function getOnnxLibName(): string {
  switch (process.platform) {
    case 'darwin': return 'libonnxruntime.dylib';
    case 'win32': return 'onnxruntime.dll';
    default: return 'libonnxruntime.so';
  }
}

async function startKnomit(label: string, playwright: any): Promise<{ instance: KnomitInstance; child: ChildProcess }> {
  const binaryPath = join(PROJECT_ROOT, 'dist', 'knomit');
  if (!existsSync(binaryPath)) {
    throw new Error(`Binary not found at ${binaryPath}. Run 'make dist' first.`);
  }

  const port = await getPort();
  const home = mkdtempSync(join(tmpdir(), `knomit-e2e-${label}-`));

  const userModelsDir = join(process.env.HOME || '~', '.knomit', 'models');
  const targetModelsDir = join(home, 'models');
  if (existsSync(userModelsDir)) {
    cpSync(userModelsDir, targetModelsDir, { recursive: true });
  }

  const onnxLib = join(PROJECT_ROOT, 'dist', 'lib', getOnnxLibName());
  const env: Record<string, string> = {
    ...process.env as Record<string, string>,
    KNOMIT_HOME: home,
    KNOMIT_PORT: String(port),
    ONNXRUNTIME_SHARED_LIBRARY: onnxLib,
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

  const branch = await discoverAgentBranch(baseURL);
  const api = await playwright.request.newContext({ baseURL });
  return { instance: { baseURL, port, home, api, pid, branch }, child };
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

/** Poll facts collection until we see at least `min` results (index sync is async). */
async function waitForFactCount(inst: KnomitInstance, min: number, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await inst.api.get(
      `${inst.baseURL}/api/v1/repos/knomit/branches/${inst.branch}/facts?limit=500`,
    );
    if (res.ok()) {
      const { count } = await res.json();
      if (count >= min) return;
    }
    await new Promise(r => setTimeout(r, 500));
  }
  throw new Error(`Timed out waiting for ${min} facts on ${inst.baseURL}`);
}

// ── Test suite ──────────────────────────────────────

const test = base.extend<{ remoteMigration: { local: KnomitInstance; remote: KnomitInstance } }>({
  remoteMigration: async ({ playwright }, use) => {
    const { instance: remote } = await startKnomit('remote', playwright);
    const { instance: local } = await startKnomit('local', playwright);

    try {
      await use({ local, remote });
    } finally {
      await local.api.dispose();
      await remote.api.dispose();
      killInstance(local.pid);
      killInstance(remote.pid);
      rmSync(local.home, { recursive: true, force: true });
      rmSync(remote.home, { recursive: true, force: true });
    }
  },
});

test.describe('Remote Migration Journey', () => {

  test('migrate local facts to a remote knomit instance', async ({ remoteMigration }) => {
    const { local, remote } = remoteMigration;

    // ── Seed remote with facts ──────────────────────
    await seedFact(remote.api, remote.baseURL, remote.branch, 'kb/networking/tcp.md',
      makeFact('TCP Protocol', 'networking', 'TCP is a reliable transport protocol.'));
    await seedFact(remote.api, remote.baseURL, remote.branch, 'kb/networking/udp.md',
      makeFact('UDP Protocol', 'networking', 'UDP is an unreliable transport protocol.'));
    await seedFact(remote.api, remote.baseURL, remote.branch, 'kb/databases/acid.md',
      makeFact('ACID Properties', 'databases', 'ACID ensures reliable transactions.'));

    // ── Seed local with facts ───────────────────────
    await seedFact(local.api, local.baseURL, local.branch, 'kb/security/tls.md',
      makeFact('TLS', 'security', 'TLS encrypts data in transit.'));
    await seedFact(local.api, local.baseURL, local.branch, 'kb/security/oauth.md',
      makeFact('OAuth 2.0', 'security', 'OAuth is an authorization framework.'));

    // Wait for index sync to catch up on both sides
    await waitForFactCount(local, 2);
    await waitForFactCount(remote, 3);

    // ── Step 1: Create session ──────────────────────
    // The remote knomit exposes a smart HTTP git endpoint at /git/knomit
    const remoteGitURL = `${remote.baseURL}/git/knomit`;

    const sessionRes = await local.api.post(`${local.baseURL}/api/v1/repos/knomit/origin-sessions`, {
      data: { url: remoteGitURL, auth_method: '' },
    });
    expect(sessionRes.ok(), `create session: ${sessionRes.status()}`).toBeTruthy();
    const { session_id } = await sessionRes.json();
    expect(session_id).toBeTruthy();

    // ── Step 2: Test connectivity ───────────────────
    const testRes = await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/test`);
    expect(testRes.ok, `test connectivity: ${testRes.status}`).toBeTruthy();
    const testEvents = await readSSE(testRes);

    // Check for error events first for better diagnostics.
    const testError = testEvents.find(e => e.phase === 'error');
    expect(testError, `test connectivity error: ${JSON.stringify(testError)}`).toBeFalsy();

    const testDone = testEvents.find(e => e.phase === 'done');
    expect(testDone, `test should have done event. Events: ${JSON.stringify(testEvents)}`).toBeTruthy();
    // History may be "shared" if both instances initialized within the same second
    // (identical root commits). Both paths are valid for migration.
    expect(['disjoint', 'shared']).toContain(testDone.result.history);
    expect(testDone.result.remote_fact_count).toBeGreaterThanOrEqual(3);
    expect(testDone.result.local_fact_count).toBeGreaterThanOrEqual(2);
    expect(testDone.result.default_branch).toBeTruthy();

    // ── Step 3: Preview ─────────────────────────────
    const previewRes = await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/preview`);
    expect(previewRes.ok).toBeTruthy();
    const previewEvents = await readSSE(previewRes);
    const previewDone = previewEvents.find(e => e.phase === 'done');
    expect(previewDone).toBeTruthy();
    if (testDone.result.history === 'disjoint') {
      expect(previewDone.result.local_only).toBeGreaterThanOrEqual(2);
      expect(previewDone.result.remote_only).toBeGreaterThanOrEqual(3);
    }

    // ── Step 4: Apply with local_wins ───────────────
    const applyRes = await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/apply`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conflict_strategy: 'local_wins', branch: testDone.result.default_branch }),
    });
    expect(applyRes.ok).toBeTruthy();
    const applyEvents = await readSSE(applyRes);
    const applyDone = applyEvents.find(e => e.phase === 'done');
    expect(applyDone).toBeTruthy();
    if (testDone.result.history === 'disjoint') {
      expect(applyDone.result.total_facts).toBeGreaterThanOrEqual(5);
      expect(applyDone.result.from_local).toBeGreaterThanOrEqual(2);
      expect(applyDone.result.from_remote).toBeGreaterThanOrEqual(3);
    }

    // ── Step 5: Commit ──────────────────────────────
    const commitRes = await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/commit`, {
      method: 'POST',
    });
    expect(commitRes.ok).toBeTruthy();
    const commitEvents = await readSSE(commitRes);
    const commitDone = commitEvents.find(e => e.phase === 'done');
    expect(commitDone, 'commit should complete').toBeTruthy();

    // Verify: swapping, configuring, rebuilding phases occurred
    expect(commitEvents.some(e => e.phase === 'swapping')).toBeTruthy();
    expect(commitEvents.some(e => e.phase === 'configuring')).toBeTruthy();
    expect(commitEvents.some(e => e.phase === 'rebuilding')).toBeTruthy();

    // ── Step 6: Verify post-migration state ─────────

    // Session should be cleaned up
    const sessionCheck = await local.api.get(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}`);
    expect(sessionCheck.status()).toBe(404);

    // Remote config should be saved
    const originRes = await local.api.get(`${local.baseURL}/api/v1/repos/knomit/origin`);
    expect(originRes.ok()).toBeTruthy();
    const originData = await originRes.json();
    expect(originData.url).toBe(remoteGitURL);
    expect(originData.branch).toBe(testDone.result.default_branch);

    // Verify facts are present (only reliable when disjoint replay ran)
    if (testDone.result.history === 'disjoint') {
      // Re-discover branch after migration (branch may have changed)
      const migratedBranch = await discoverAgentBranch(local.baseURL);
      const recentRes = await local.api.get(
        `${local.baseURL}/api/v1/repos/knomit/branches/${migratedBranch}/facts?limit=100`,
      );
      expect(recentRes.ok()).toBeTruthy();
      const recent = await recentRes.json();
      const paths = recent._embedded.facts.map((f: any) => f.path);
      // Remote facts
      expect(paths).toContain('kb/networking/tcp.md');
      expect(paths).toContain('kb/networking/udp.md');
      expect(paths).toContain('kb/databases/acid.md');
      // Local facts
      expect(paths).toContain('kb/security/tls.md');
      expect(paths).toContain('kb/security/oauth.md');

      // Verify individual fact content
      const tcpRes = await local.api.get(
        `${local.baseURL}/api/v1/repos/knomit/branches/${migratedBranch}/facts/kb/networking/tcp.md`,
      );
      expect(tcpRes.ok()).toBeTruthy();
      const tcpData = await tcpRes.json();
      expect(tcpData.body).toContain('reliable transport protocol');
    }
  });

  test('remote_wins strategy preserves remote content on shared paths', async ({ remoteMigration }) => {
    const { local, remote } = remoteMigration;

    const sharedPath = 'kb/shared/protocol.md';

    // Both instances have the same path with different content
    await seedFact(remote.api, remote.baseURL, remote.branch, sharedPath,
      makeFact('Remote Protocol', 'networking', 'Remote version of the protocol.'));
    await seedFact(local.api, local.baseURL, local.branch, sharedPath,
      makeFact('Local Protocol', 'networking', 'Local version of the protocol.'));

    // Also seed unique facts on each side
    await seedFact(remote.api, remote.baseURL, remote.branch, 'kb/remote-only/data.md',
      makeFact('Remote Data', 'databases', 'Only on remote.'));
    await seedFact(local.api, local.baseURL, local.branch, 'kb/local-only/data.md',
      makeFact('Local Data', 'security', 'Only on local.'));

    await waitForFactCount(local, 2);
    await waitForFactCount(remote, 2);

    const remoteGitURL = `${remote.baseURL}/git/knomit`;

    // Create session → test → preview
    const sessRes = await local.api.post(`${local.baseURL}/api/v1/repos/knomit/origin-sessions`, {
      data: { url: remoteGitURL, auth_method: '' },
    });
    const { session_id } = await sessRes.json();

    const testEvents2 = await readSSE(await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/test`));
    const testDone2 = testEvents2.find(e => e.phase === 'done');
    expect(testDone2, `test events: ${JSON.stringify(testEvents2)}`).toBeTruthy();

    await readSSE(await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/preview`));

    // Apply with remote_wins
    const applyRes = await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/apply`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conflict_strategy: 'remote_wins' }),
    });
    const applyEvents = await readSSE(applyRes);
    const applyDone = applyEvents.find(e => e.phase === 'done');
    expect(applyDone, `no done in apply events: ${JSON.stringify(applyEvents)}`).toBeTruthy();

    // Commit
    const commitEvents = await readSSE(await fetch(`${local.baseURL}/api/v1/repos/knomit/origin-sessions/${session_id}/commit`, { method: 'POST' }));
    const commitError = commitEvents.find(e => e.phase === 'error');
    expect(commitError, `commit error: ${JSON.stringify(commitError)}`).toBeFalsy();

    // After commit, the remote config should be saved
    const originRes = await local.api.get(`${local.baseURL}/api/v1/repos/knomit/origin`);
    expect(originRes.ok()).toBeTruthy();
    const originData = await originRes.json();
    expect(originData.url).toBe(remoteGitURL);

    // If disjoint history (replay path), verify fact content
    if (testDone2.result.history === 'disjoint') {
      // Re-discover branch after migration
      const migratedBranch = await discoverAgentBranch(local.baseURL);
      // Shared path should have REMOTE content with remote_wins
      const factRes = await local.api.get(
        `${local.baseURL}/api/v1/repos/knomit/branches/${migratedBranch}/facts/${sharedPath}`,
      );
      expect(factRes.ok(), `shared fact: ${factRes.status()}`).toBeTruthy();
      const factData = await factRes.json();
      expect(factData.body).toContain('Remote version');

      // Local-only fact should still exist
      const localOnlyRes = await local.api.get(
        `${local.baseURL}/api/v1/repos/knomit/branches/${migratedBranch}/facts/kb/local-only/data.md`,
      );
      expect(localOnlyRes.ok()).toBeTruthy();
    }
  });

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
