import { spawn } from 'node:child_process';
import { cpSync, existsSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import getPort from 'get-port';
import { knomitModelsDir } from './helpers/appdirs.js';
import { onnxEnv, requireBuild } from './helpers/binary.js';
import { cleanupFromState, STATE_FILE, type E2EState } from './helpers/cleanup.js';
import { createRepo, seedFixture } from './fixtures/seed.js';

function writeState(state: E2EState) {
  writeFileSync(STATE_FILE, JSON.stringify(state, null, 2));
}

async function start() {
  // 1. Use the existing build; fail with the commands to run by hand when absent.
  const binaryPath = requireBuild();

  // A state file left by a crashed earlier run would be overwritten below and
  // its server and home leaked: clean it first.
  cleanupFromState();

  // 2. Find a free port
  const port = await getPort();

  // 3. Create temp KNOMIT_HOME and record it at once, so a failure from here
  //    on is cleaned up (Playwright does not run globalTeardown when setup throws).
  const knomitHome = mkdtempSync(join(tmpdir(), 'knomit-e2e-'));
  writeState({ home: knomitHome, port });
  console.log(`[e2e] KNOMIT_HOME: ${knomitHome}`);

  // 4. Copy ONNX model cache if available
  const userModelsDir = knomitModelsDir();
  const targetModelsDir = join(knomitHome, 'models');
  if (existsSync(userModelsDir)) {
    console.log('[e2e] Copying ONNX model cache...');
    cpSync(userModelsDir, targetModelsDir, { recursive: true });
  }

  // 5. Start the knomit binary
  const env: Record<string, string> = {
    ...process.env as Record<string, string>,
    KNOMIT_HOME: knomitHome,
    KNOMIT_PORT: String(port),
    ...onnxEnv(),
  };

  console.log(`[e2e] Starting knomit (${binaryPath}) on port ${port}...`);
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

  // Record the pid before anything else can fail.
  const baseURL = `http://localhost:${port}`;
  writeState({ pid, port, home: knomitHome, baseURL });

  // 6. Poll GET /api/v1/repos until 200
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

  // 9. Set env for Playwright
  process.env.KNOMIT_E2E_BASE_URL = baseURL;
}

async function globalSetup() {
  try {
    await start();
  } catch (err) {
    // Playwright skips globalTeardown when setup throws: clean up here.
    cleanupFromState();
    throw err;
  }
}

export default globalSetup;
