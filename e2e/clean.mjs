// Removes what an aborted e2e run leaves behind (`make e2e-clean`).
//
// Kills ONLY `knomit serve` processes that were started from THIS checkout's
// binary (dist/knomit, .e2e-bin/knomit, or E2E_BINARY) AND carry a
// KNOMIT_HOME under a knomit-e2e-* temp dir. Anything else, including a user
// instance on port 19278, is never touched. Then deletes the knomit-e2e-* and
// knomit-remote-* homes in the temp dir that no surviving process still uses.
import { execFileSync } from 'node:child_process';
import { readdirSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dirname, '..');
const binaries = new Set(
  [join(root, 'dist', 'knomit'), join(root, '.e2e-bin', 'knomit'), process.env.E2E_BINARY && resolve(process.env.E2E_BINARY)].filter(Boolean),
);

// BSD ps (darwin) prints the environment with -E; procps (linux) with the `e` modifier.
const psArgs = process.platform === 'darwin' ? ['-Eww', '-axo', 'pid=,command='] : ['eww', '-axo', 'pid=,command='];
const lines = execFileSync('ps', psArgs, { encoding: 'utf-8', maxBuffer: 256 * 1024 * 1024 }).split('\n');

const liveHomes = [];
let killed = 0;
for (const line of lines) {
  const m = line.match(/^\s*(\d+)\s+(\S+) serve(\s|$)/);
  const home = line.match(/KNOMIT_HOME=(\S*knomit-e2e-[^\s/]*)/);
  if (!m) {
    const any = line.match(/KNOMIT_HOME=(\S+)/);
    if (any && line.includes(' serve')) liveHomes.push(any[1]);
    continue;
  }
  if (binaries.has(m[2]) && home) {
    try {
      process.kill(Number(m[1]), 'SIGTERM');
      killed++;
      console.log(`killed knomit serve pid ${m[1]} (${m[2]})`);
    } catch (err) {
      console.warn(`could not kill pid ${m[1]}: ${err}`);
    }
  } else if (home) {
    liveHomes.push(home[1]);
  }
}

await new Promise((r) => setTimeout(r, 500));
let removed = 0;
const tmp = tmpdir();
for (const name of readdirSync(tmp)) {
  if (!/^knomit-(e2e|remote)-/.test(name)) continue;
  const dir = join(tmp, name);
  if (liveHomes.some((h) => h === dir || h.startsWith(dir + '/'))) {
    console.log(`kept ${dir} (held by a process not started from this checkout)`);
    continue;
  }
  try {
    if (!statSync(dir).isDirectory()) continue;
    rmSync(dir, { recursive: true, force: true });
    removed++;
  } catch (err) {
    console.warn(`could not remove ${dir}: ${err}`);
  }
}
rmSync(join(import.meta.dirname, '.e2e-state.json'), { force: true });
console.log(`e2e-clean: killed ${killed} server(s), removed ${removed} temp home(s)`);
