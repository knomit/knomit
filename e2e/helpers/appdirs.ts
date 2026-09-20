/**
 * The knomit data root, per platform — the TypeScript mirror of
 * internal/apppaths.
 *
 * The harness never runs knomit out of this directory: every spawn sets
 * KNOMIT_HOME to a fresh temp dir. It needs the real one for exactly one
 * reason, which is to COPY the cached ONNX model out of it. Miss, and the run
 * re-downloads 617MB before the first assertion.
 *
 * That is how the previous version failed on Windows. It read
 * `process.env.HOME`, which is a POSIX variable that Windows does not set
 * outside a Git Bash / MSYS shell, and fell back to the literal string '~' —
 * a directory name, not an expansion. existsSync('~/.knomit/models') is always
 * false, so the copy was skipped silently and every Windows run paid the
 * download. os.homedir() is the cross-platform answer and consults
 * %USERPROFILE% on Windows.
 */

import { homedir } from 'node:os';
import { join } from 'node:path';

/**
 * knomitHome mirrors apppaths.DefaultHome(): %LOCALAPPDATA%\knomit\home on
 * Windows, ~/.knomit everywhere else. Keep it in step with
 * internal/apppaths — the Go side is the source of truth.
 */
export function knomitHome(): string {
  if (process.platform === 'win32') {
    const local = process.env.LOCALAPPDATA ?? join(homedir(), 'AppData', 'Local');
    return join(local, 'knomit', 'home');
  }
  return join(homedir(), '.knomit');
}

/** knomitModelsDir is the ONNX model cache inside the data root. */
export function knomitModelsDir(): string {
  return join(knomitHome(), 'models');
}
