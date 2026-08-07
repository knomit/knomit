import { execSync, spawn } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import getPort from 'get-port';
import { createRepo, seedFixture } from './fixtures/seed.js';

const PROJECT_ROOT = resolve(import.meta.dirname, '..');
const STATE_FILE = resolve(import.meta.dirname, '.e2e-state.json');

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

async function globalSetup() {
  // 1. Build the binary
  console.log('[e2e] Building knomit binary via make dist...');
  execSync('make dist', { cwd: PROJECT_ROOT, stdio: 'inherit' });

  const binaryPath = join(PROJECT_ROOT, 'dist', 'knomit');
  if (!existsSync(binaryPath)) {
    throw new Error(`Binary not found at ${binaryPath}`);
  }

  // 2. Find a free port
  const port = await getPort();

  // 3. Create temp KNOMIT_HOME
  const knomitHome = mkdtempSync(join(tmpdir(), 'knomit-e2e-'));
  console.log(`[e2e] KNOMIT_HOME: ${knomitHome}`);

  // 4. Copy ONNX model cache if available
  const userModelsDir = join(process.env.HOME || '~', '.knomit', 'models');
  const targetModelsDir = join(knomitHome, 'models');
  if (existsSync(userModelsDir)) {
    console.log('[e2e] Copying ONNX model cache...');
    cpSync(userModelsDir, targetModelsDir, { recursive: true });
  }

  // 5. Start the knomit binary
  const onnxLib = join(PROJECT_ROOT, 'dist', 'lib', getOnnxLibName());
  const env: Record<string, string> = {
    ...process.env as Record<string, string>,
    KNOMIT_HOME: knomitHome,
    KNOMIT_PORT: String(port),
    ONNXRUNTIME_SHARED_LIBRARY: onnxLib,
  };

  console.log(`[e2e] Starting knomit on port ${port}...`);
  const child = spawn(binaryPath, ['serve'], {
    env,
    detached: true,
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  child.stdout?.on('data', (data: Buffer) => {
    for (const line of data.toString().split('\n').filter(Boolean)) {
      console.log(`[knomit] ${line}`);
    }
  });
  child.stderr?.on('data', (data: Buffer) => {
    for (const line of data.toString().split('\n').filter(Boolean)) {
      console.error(`[knomit] ${line}`);
    }
  });

  child.unref();

  const pid = child.pid;
  if (!pid) {
    throw new Error('Failed to start knomit process');
  }

  // 6. Poll GET /api/v1/repos until 200
  const baseURL = `http://localhost:${port}`;
  const deadline = Date.now() + 60_000;
  let ready = false;

  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${baseURL}/api/v1/repos`);
      if (res.ok) {
        ready = true;
        break;
      }
    } catch {
      // Server not up yet
    }
    await new Promise((r) => setTimeout(r, 500));
  }

  if (!ready) {
    // Clean up on failure
    try { process.kill(-pid); } catch { /* ignore */ }
    throw new Error('knomit server did not become ready within 60s');
  }

  console.log('[e2e] Server is ready');

  // 7. Create the repo the tests work in. A fresh knomit serves none.
  console.log('[e2e] Creating e2e repo...');
  await createRepo(baseURL);

  // 8. Seed data
  console.log('[e2e] Seeding fixture data...');
  await seedFixture(baseURL);
  console.log('[e2e] Seed complete');

  // 9. Write state file
  const state = { pid, port, home: knomitHome, baseURL };
  writeFileSync(STATE_FILE, JSON.stringify(state, null, 2));

  // 10. Set env for Playwright
  process.env.KNOMIT_E2E_BASE_URL = baseURL;
}

export default globalSetup;
