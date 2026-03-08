# Vendored Git Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Bundle dugite-native git binaries per platform so knomit works without system git installed.

**Architecture:** Two changes: (1) update `src/git.ts` resolution chain to find vendored git at `vendor/git/bin/git` and set env vars, (2) add `downloadGit()` to `scripts/build-all.ts` to fetch and strip dugite-native during builds.

**Tech Stack:** dugite-native v2.53.0, Bun, TypeScript

---

### Task 1: Update vendored git path and env vars in `src/git.ts`

**Files:**
- Modify: `src/git.ts:46-100`

**Step 1: Write a test for vendored git env vars**

Create `src/git.test.ts` (or add to existing test file). We need to verify that when using vendored git, the correct env vars are set. Since we can't easily mock `Bun.spawnSync`, write a unit test for a new helper function that builds the env:

```ts
// src/git.test.ts
import { describe, it, expect } from "bun:test";
import { vendoredGitEnv } from "./git";

describe("vendoredGitEnv", () => {
  it("returns env vars when bin is under vendor/git/", () => {
    const env = vendoredGitEnv("/app/vendor/git/bin/git");
    expect(env).toEqual({
      GIT_EXEC_PATH: "/app/vendor/git/libexec/git-core",
      GIT_TEMPLATE_DIR: "/app/vendor/git/share/git-core/templates",
      GIT_SSL_CAINFO: "/app/vendor/git/ssl/cacert.pem",
    });
  });

  it("returns null when bin is system git", () => {
    const env = vendoredGitEnv("/usr/bin/git");
    expect(env).toBeNull();
  });
});
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test git.test.ts`
Expected: FAIL — `vendoredGitEnv` is not exported

**Step 3: Implement `vendoredGitEnv` and update `resolveGitBin` and `git()`**

In `src/git.ts`, add the exported helper and update two methods:

```ts
/** Returns extra env vars needed when using vendored git, or null for system git. */
export function vendoredGitEnv(gitBin: string): Record<string, string> | null {
  const marker = join("vendor", "git", "bin", "git");
  const idx = gitBin.indexOf(marker);
  if (idx === -1) return null;
  const vendorGitDir = gitBin.slice(0, idx + "vendor/git".length);
  return {
    GIT_EXEC_PATH: join(vendorGitDir, "libexec", "git-core"),
    GIT_TEMPLATE_DIR: join(vendorGitDir, "share", "git-core", "templates"),
    GIT_SSL_CAINFO: join(vendorGitDir, "ssl", "cacert.pem"),
  };
}
```

Update `resolveGitBin()` — change line 59 from:
```ts
const vendored = join(execDir, "vendor", "git");
```
to:
```ts
const vendored = join(execDir, "vendor", "git", "bin", "git");
```

Update the error message (line 66) from:
```ts
"Git binary not found. Install git or place a static binary at <exec_dir>/vendor/git"
```
to:
```ts
"Git not found. Install git or use a platform build with bundled git."
```

Update `git()` method — change line 78 from:
```ts
const proc = Bun.spawnSync([bin, "-C", this.repoPath, ...args]);
```
to:
```ts
const extraEnv = vendoredGitEnv(bin);
const proc = Bun.spawnSync([bin, "-C", this.repoPath, ...args], extraEnv ? { env: { ...process.env, ...extraEnv } } : undefined);
```

Also update the bare `Bun.spawnSync` in `init()` (line 135) from:
```ts
Bun.spawnSync([bin, "init", this.repoPath]);
```
to:
```ts
const extraEnv = vendoredGitEnv(bin);
Bun.spawnSync([bin, "init", this.repoPath], extraEnv ? { env: { ...process.env, ...extraEnv } } : undefined);
```

**Step 4: Run test to verify it passes**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test git.test.ts`
Expected: PASS

**Step 5: Run full test suite**

Run: `cd /Users/knomit/data/mine/knomit/src && bun test`
Expected: all tests pass (existing git tests use system git, unaffected)

**Step 6: Commit**

```bash
git add src/git.ts src/git.test.ts
git commit -m "feat: update vendored git resolution with env var support"
```

---

### Task 2: Add `downloadGit()` to build script

**Files:**
- Modify: `scripts/build-all.ts:144-171`

**Step 1: Add dugite platform mapping and constants**

After the existing `sqliteLibName()` function (line 44), add:

```ts
const DUGITE_VERSION = "v2.53.0";

function dugiteAssetName(target: Target): string {
  const platMap: Record<string, string> = {
    "darwin-arm64": "macOS-arm64",
    "linux-x64": "ubuntu-x64",
    "linux-arm64": "ubuntu-arm64",
    "win32-x64": "windows-x64",
  };
  const plat = platMap[`${target.platform}-${target.arch}`];
  if (!plat) throw new Error(`No dugite-native build for ${target.platform}-${target.arch}`);
  // dugite-native asset names include a short commit hash that varies per release.
  // We use a glob pattern to match, but for the URL we need the exact name.
  // The release page lists assets — we'll fetch the release API to find the exact name.
  return plat;
}
```

**Step 2: Add `downloadGit()` function**

After `copySqliteLib()`:

```ts
const DUGITE_BLACKLIST = [
  "git-lfs",
  "git-credential-manager",
  "git-svn",
  "git-p4",
  "git-gui",
  "gitk",
  "git-daemon",
  "git-shell",
  "git-http-backend",
  "git-cvsserver",
  "git-cvsimport",
  "git-cvsexportcommit",
  "git-send-email",
  "git-request-pull",
  "git-instaweb",
  "git-archimport",
  "scalar",
];

async function downloadGit(target: Target, vendorDir: string) {
  // Fetch release assets list to find exact filename (includes commit hash)
  const releaseUrl = `https://api.github.com/repos/desktop/dugite-native/releases/tags/${DUGITE_VERSION}`;
  log(`fetching dugite-native release info from ${releaseUrl}`);
  const releaseResp = await fetch(releaseUrl);
  if (!releaseResp.ok) throw new Error(`Failed to fetch release info: HTTP ${releaseResp.status}`);
  const release = await releaseResp.json() as { assets: Array<{ name: string; browser_download_url: string }> };

  const platKey = dugiteAssetName(target);
  // Match asset: dugite-native-<version>-<hash>-<platform>.tar.gz
  const asset = release.assets.find((a: { name: string }) =>
    a.name.includes(platKey) && a.name.endsWith(".tar.gz") && !a.name.includes("lzma")
  );
  if (!asset) throw new Error(`No dugite-native asset found for ${platKey}`);

  log(`downloading vendored git from ${asset.browser_download_url}`);
  const resp = await fetch(asset.browser_download_url);
  if (!resp.ok) throw new Error(`Failed to download git: HTTP ${resp.status}`);

  mkdirSync(vendorDir, { recursive: true });
  const tarPath = join(vendorDir, "git.tar.gz");
  await Bun.write(tarPath, await resp.arrayBuffer());

  // Extract — dugite-native tarballs contain a top-level git/ directory
  run(["tar", "xzf", tarPath, "-C", vendorDir]);
  rmSync(tarPath);

  // Strip blacklisted components
  const gitCoreDir = join(vendorDir, "git", "libexec", "git-core");
  for (const name of DUGITE_BLACKLIST) {
    const glob = new Bun.Glob(`${name}*`);
    for (const entry of glob.scanSync({ cwd: gitCoreDir })) {
      const fullPath = join(gitCoreDir, entry);
      try {
        rmSync(fullPath, { recursive: true, force: true });
        log(`stripped ${entry}`);
      } catch { /* ignore */ }
    }
  }

  // Strip non-essential directories
  for (const dir of ["share/gitweb", "share/perl5"]) {
    const fullPath = join(vendorDir, "git", dir);
    try {
      rmSync(fullPath, { recursive: true, force: true });
      log(`stripped ${dir}`);
    } catch { /* ignore */ }
  }

  log(`installed vendored git ${DUGITE_VERSION}`);
}
```

**Step 3: Add step to `buildTarget()`**

In `buildTarget()`, after Step 4 (copySqliteLib) and before Step 5 (createTarball), add:

```ts
  // Step 5: Download vendored git
  const vendorDir = join(outDir, "vendor");
  await downloadGit(target, vendorDir);

  // Step 6: Create tarball (renumber from Step 5)
  createTarball(target);
```

**Step 4: Test the build**

Run: `cd /Users/knomit/data/mine/knomit && bun scripts/build-all.ts --platform $(uname -s | tr 'A-Z' 'a-z')-$(uname -m | sed 's/x86_64/x64/' | sed 's/aarch64/arm64/')`
Expected: build completes, `dist/<platform>/knomit/vendor/git/bin/git` exists

**Step 5: Verify vendored git works**

Run: `dist/<platform>/knomit/vendor/git/bin/git --version`
Expected: `git version 2.53.0`

**Step 6: Verify blacklist was applied**

Run: `ls dist/<platform>/knomit/vendor/git/libexec/git-core/ | grep -E 'git-lfs|git-svn|git-p4|scalar'`
Expected: no output (all stripped)

**Step 7: Commit**

```bash
git add scripts/build-all.ts
git commit -m "feat: add dugite-native git download to build pipeline"
```

---

### Task 3: Smoke test vendored git end-to-end

**Step 1: Run the compiled binary with vendored git**

After a successful build from Task 2, test the full resolution chain. Temporarily rename system git to force vendored fallback:

```bash
# Verify the built binary can use vendored git
cd dist/<platform>/knomit
./knomit --help
# Should work — TUI won't start without a terminal, but help should print
```

**Step 2: Verify git init with vendored git**

```bash
# Set PATH to exclude system git, use only the built binary
env PATH="$(pwd)/dist/<platform>/knomit:$PATH" \
  KNOMIT_REPO=/tmp/knomit-vendor-test \
  knomit reset 2>/dev/null
env PATH="$(pwd)/dist/<platform>/knomit:$PATH" \
  KNOMIT_REPO=/tmp/knomit-vendor-test \
  knomit mcp --help
```

This verifies the binary resolves vendored git and can operate.

**Step 3: Clean up**

```bash
rm -rf /tmp/knomit-vendor-test
```

**Step 4: Commit any fixes**

If any issues were found and fixed, commit them.

---

### Task 4: Update research doc

**Files:**
- Modify: `spec/research/vendored-git.md` (on the research branch — skip if not needed)
- Modify: `docs/plans/2026-03-08-vendored-git-design.md`

**Step 1: Add the git commands audit to the design doc**

Append a "Git commands audit" section listing all current + future commands needed, and the blacklist rationale.

**Step 2: Commit**

```bash
git add -f docs/plans/2026-03-08-vendored-git-design.md
git commit -m "docs: add git commands audit to vendored git design"
```
