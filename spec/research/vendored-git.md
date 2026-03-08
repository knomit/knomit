# Vendoring Git with Knomit (dugite-native)

## Goal

Ship a bundled git binary with knomit so users don't need git installed on their system. Same pattern as the bundled SQLite, sqlite-vec, and ONNX runtime in `lib/`.

## What Already Exists

### Resolution chain (`src/git.ts:46-67`)

```
1. System git    →  which git
2. Vendored git  →  <exec_dir>/vendor/git
3. Error         →  "Install git or place a static binary at <exec_dir>/vendor/git"
```

Step 2 already works — we just need to populate `vendor/` at build time.

### Build infrastructure (`scripts/build-all.ts`)

The build script already:
- Cross-compiles for 4 targets (darwin-arm64, linux-x64, linux-arm64, win32-x64)
- Downloads platform-specific native libs (sqlite-vec from GitHub releases)
- Copies native libs (ONNX, SQLite) into `lib/`
- Packages everything into tarballs

Adding git is a new step in the same pipeline.

## Source: dugite-native

[dugite-native](https://github.com/desktop/dugite-native) is GitHub Desktop's portable git distribution. It builds git specifically for embedding in applications — server-side commands stripped, client-focused.

### Current release: v2.53.0

| Platform | Compressed (tar.gz) | Compressed (lzma) | Knomit target |
|----------|---------------------|-------------------|---------------|
| macOS arm64 | 60 MB | 39 MB | `darwin-arm64` |
| macOS x64 | 63 MB | 44 MB | (not a target) |
| Linux x64 | 63 MB | 42 MB | `linux-x64` |
| Linux arm64 | 23 MB | 11 MB | `linux-arm64` |
| Windows x64 | 48 MB | 29 MB | `win32-x64` |

Note: macOS and Linux x64 bundles are larger because they include Git LFS and Git Credential Manager. The lzma variants are significantly smaller.

### What's inside a dugite-native tarball

```
git/
├── bin/
│   └── git                    # the main binary
├── libexec/git-core/          # internal git commands
│   ├── git-remote-https
│   ├── git-lfs                # (optional, included)
│   └── ...
├── share/git-core/templates/  # git templates
├── etc/gitconfig              # default config
└── ssl/cacert.pem             # CA certs for HTTPS
```

git needs `libexec/git-core/` relative to `bin/git` for subcommands (remote, merge, etc.) to work. This means we vendor the entire `git/` directory, not just the binary.

### Git version: 2.53.0

Well past the 2.34 minimum for SSH signing. Maintained by GitHub — regular releases tracking upstream git.

## Implementation Plan

### 1. Update directory layout

Current distribution:
```
knomit/
├── knomit              # executable
└── lib/
    ├── libsqlite3.dylib
    ├── vec0.dylib
    ├── onnxruntime_binding.node
    └── libonnxruntime.dylib
```

With vendored git:
```
knomit/
├── knomit              # executable
├── lib/
│   ├── libsqlite3.dylib
│   ├── vec0.dylib
│   ├── onnxruntime_binding.node
│   └── libonnxruntime.dylib
└── vendor/
    └── git/
        ├── bin/git
        ├── libexec/git-core/
        ├── share/git-core/templates/
        ├── etc/gitconfig
        └── ssl/cacert.pem
```

### 2. Update `src/git.ts` — resolve vendored git path

Current code looks for `<exec_dir>/vendor/git` (a single binary). Update to look for `<exec_dir>/vendor/git/bin/git` instead, since dugite-native has a directory structure.

```diff
 // Try vendored git
 const execDir = dirname(Bun.execPath);
-const vendored = join(execDir, "vendor", "git");
+const vendored = join(execDir, "vendor", "git", "bin", "git");
 if (await exists(vendored)) {
   this.gitBin = vendored;
   return this.gitBin;
 }
```

Set `GIT_EXEC_PATH` so git finds its subcommands:

```diff
 private async git(...args: string[]): Promise<{ stdout: string; stderr: string; exitCode: number }> {
   const bin = await this.resolveGitBin();
   log.debug(`git ${args.join(" ")}`);
-  const proc = Bun.spawnSync([bin, "-C", this.repoPath, ...args]);
+  const env = { ...process.env };
+  // If using vendored git, set GIT_EXEC_PATH so it finds subcommands
+  const vendorDir = join(dirname(Bun.execPath), "vendor", "git");
+  if (bin.startsWith(vendorDir)) {
+    env.GIT_EXEC_PATH = join(vendorDir, "libexec", "git-core");
+    env.GIT_TEMPLATE_DIR = join(vendorDir, "share", "git-core", "templates");
+    env.GIT_SSL_CAINFO = join(vendorDir, "ssl", "cacert.pem");
+  }
+  const proc = Bun.spawnSync([bin, "-C", this.repoPath, ...args], { env });
   // ... rest unchanged
 }
```

### 3. Add download step to `scripts/build-all.ts`

Add a new function alongside `downloadVecExtension`:

```ts
const DUGITE_VERSION = "v2.53.0";
const DUGITE_COMMIT = "6981a1f"; // from release asset names

function dugitePlatform(target: Target): string {
  if (target.platform === "darwin") return `macOS-${target.arch}`;
  if (target.platform === "win32") return `windows-${target.arch === "x64" ? "x64" : "x86"}`;
  return `ubuntu-${target.arch}`;
}

async function downloadGit(target: Target, vendorDir: string) {
  const plat = dugitePlatform(target);
  const filename = `dugite-native-${DUGITE_VERSION}-${DUGITE_COMMIT}-${plat}.tar.gz`;
  const url = `https://github.com/desktop/dugite-native/releases/download/${DUGITE_VERSION}/${filename}`;

  log(`downloading vendored git from ${url}`);
  const resp = await fetch(url);
  if (!resp.ok) throw new Error(`Failed to download git: HTTP ${resp.status}`);

  const tarPath = join(vendorDir, "git.tar.gz");
  mkdirSync(vendorDir, { recursive: true });
  await Bun.write(tarPath, await resp.arrayBuffer());

  // Extract — dugite-native tarballs contain a top-level git/ directory
  run(["tar", "xzf", tarPath, "-C", vendorDir]);
  rmSync(tarPath);

  log(`installed vendored git ${DUGITE_VERSION}`);
}
```

Call it in `buildTarget`:

```ts
async function buildTarget(target: Target) {
  // ... existing steps ...

  // Step 5: Download vendored git
  const vendorDir = join(outDir, "vendor");
  await downloadGit(target, vendorDir);

  // Step 6: Create tarball
  createTarball(target);
}
```

### 4. Update `src/paths.ts`

Add a helper for the vendor directory:

```ts
export function vendorDir(): string {
  return join(dirname(process.execPath), "vendor");
}

export function vendoredGitDir(): string {
  return join(vendorDir(), "git");
}
```

### 5. Keep system git as primary

The resolution order stays: system git first, vendored second. This means:
- Users with their own git (and their own SSH keys configured) get that by default
- The vendored git is a fallback for systems without git installed
- Developers can override by removing/renaming the vendored directory

## Size Impact

| Platform | Current dist (approx) | + vendored git (lzma) | + vendored git (tar.gz) |
|----------|----------------------|----------------------|------------------------|
| darwin-arm64 | ~50 MB | +39 MB | +60 MB |
| linux-x64 | ~45 MB | +42 MB | +63 MB |
| linux-arm64 | ~40 MB | +11 MB | +23 MB |
| win32-x64 | ~45 MB | +29 MB | +48 MB |

The ONNX runtime alone is ~20-40 MB, so this is in the same ballpark. Consider using lzma compression for the git download and extracting at build time to reduce distribution size.

### Possible size reduction

dugite-native includes Git LFS and Git Credential Manager, neither of which knomit needs. A post-extraction cleanup step could remove:
- `libexec/git-core/git-lfs` (~10 MB)
- `libexec/git-core/git-credential-manager` (~varies)
- Server-side commands if any remain

This could reduce the Linux arm64 bundle to under 15 MB.

## Environment Variables

When using vendored git, set these so git finds its pieces:

| Variable | Value | Purpose |
|----------|-------|---------|
| `GIT_EXEC_PATH` | `<vendor>/git/libexec/git-core` | Find subcommands (git-remote-https, etc.) |
| `GIT_TEMPLATE_DIR` | `<vendor>/git/share/git-core/templates` | Init templates |
| `GIT_SSL_CAINFO` | `<vendor>/git/ssl/cacert.pem` | HTTPS CA certificates |

Do NOT set `GIT_CONFIG_SYSTEM` — let the user's system config work normally.

## SSH Signing with Vendored Git

Vendored git (2.53.0) fully supports SSH signing. The only external dependency is `ssh-keygen`, which ships with OpenSSH on all target platforms:
- **macOS**: Included with the OS
- **Linux**: Part of `openssh-client` (installed on virtually all systems)
- **Windows**: Ships with Windows 10+ (OpenSSH client is a default feature)

No additional vendoring needed for SSH signing to work.

## Testing

1. **Build test**: Run `bun run scripts/build-all.ts --platform linux-x64`, verify `vendor/git/bin/git --version` works from the output directory
2. **Integration test**: Add a test that creates a `GitRepo` with no system git in PATH, confirms it falls back to vendored git
3. **Signing test**: Verify `git commit -S` works with vendored git + system ssh-keygen
4. **HTTPS test**: Verify `git clone https://...` works (needs `GIT_SSL_CAINFO` set correctly)

## Tasks

- [ ] Update `src/git.ts` — change vendored path to `vendor/git/bin/git`, set env vars
- [ ] Update `src/paths.ts` — add `vendorDir()` and `vendoredGitDir()` helpers
- [ ] Update `scripts/build-all.ts` — add `downloadGit()` step
- [ ] Add post-extraction cleanup to strip git-lfs and git-credential-manager
- [ ] Add integration test for vendored git fallback
- [ ] Test SSH signing with vendored git
- [ ] Test HTTPS operations with vendored CA bundle
- [ ] Update error message in `resolveGitBin` (no longer "place a static binary")
