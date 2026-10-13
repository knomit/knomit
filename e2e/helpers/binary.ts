/**
 * Locates the knomit binary the harness launches and the prebuilt web bundle
 * it embeds. The harness never builds anything by default: building runs
 * `npm ci` in web/, which empties node_modules through a symlinked worktree.
 */
import { execSync } from 'node:child_process';
import { existsSync, realpathSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';

export const PROJECT_ROOT = resolve(import.meta.dirname, '..', '..');

export function onnxLibName(): string {
  switch (process.platform) {
    case 'darwin':
      return 'libonnxruntime.dylib';
    case 'win32':
      return 'onnxruntime.dll';
    default:
      return 'libonnxruntime.so';
  }
}

/** E2E_BINARY overrides the default dist/knomit. */
export function binaryPath(): string {
  return process.env.E2E_BINARY ? resolve(process.env.E2E_BINARY) : join(PROJECT_ROOT, 'dist', 'knomit');
}

/** Absolute path of the ONNX runtime next to the (symlink-resolved) binary, or undefined. */
export function onnxLibPath(): string | undefined {
  try {
    const lib = join(dirname(realpathSync(binaryPath())), 'lib', onnxLibName());
    return existsSync(lib) ? lib : undefined;
  } catch {
    return undefined;
  }
}

/** Env entries for a spawned `knomit serve`: ONNX runtime location when known. */
export function onnxEnv(): Record<string, string> {
  const lib = onnxLibPath();
  return lib ? { ONNXRUNTIME_SHARED_LIBRARY: lib } : {};
}

/**
 * Returns the binary path, or throws naming the commands to run by hand.
 * E2E_BUILD=1 opts back in to `make server` (which runs `npm ci`; `make dist` would also build the desktop app).
 */
export function requireBuild(): string {
  if (process.env.E2E_BUILD === '1') {
    console.log('[e2e] E2E_BUILD=1: running make server (this runs npm ci in web/)...');
    execSync('make server', { cwd: PROJECT_ROOT, stdio: 'inherit' });
  }
  const bin = binaryPath();
  const webIndex = join(PROJECT_ROOT, 'web', 'dist', 'index.html');
  const missing: string[] = [];
  if (!existsSync(bin)) missing.push(`knomit binary (${bin})`);
  if (!existsSync(webIndex)) missing.push(`web bundle (${webIndex})`);
  if (missing.length > 0) {
    throw new Error(
      `[e2e] Missing build input: ${missing.join(', ')}.\n` +
        `The harness does not build. Run by hand from the repo root:\n` +
        `  1. cd web && npx tsc -b && npx vite build\n` +
        `  2. CGO_ENABLED=1 go build -o dist/knomit .   (or: make server)\n` +
        `Or rerun with E2E_BUILD=1 to run 'make server' (runs npm ci; never do this in a worktree with a symlinked web/node_modules).\n` +
        `Set E2E_BINARY=<path> to launch a binary other than dist/knomit.`,
    );
  }
  return bin;
}
