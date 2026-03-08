#!/usr/bin/env bun
import { mkdirSync, rmSync, cpSync } from "node:fs";
import { join, basename } from "node:path";
import { getVecExtensionUrl, extensionFilename } from "../src/assets";

const ROOT = join(import.meta.dir, "..");
const DIST = join(ROOT, "dist");

const TARGETS = [
  { platform: "darwin", arch: "arm64", bunTarget: "bun-darwin-arm64" },
  { platform: "linux", arch: "x64", bunTarget: "bun-linux-x64" },
  { platform: "linux", arch: "arm64", bunTarget: "bun-linux-arm64" },
  { platform: "win32", arch: "x64", bunTarget: "bun-windows-x64" },
] as const;

type Target = (typeof TARGETS)[number];

function log(msg: string) {
  console.log(`[build] ${msg}`);
}

function run(cmd: string[], cwd?: string): void {
  const result = Bun.spawnSync(cmd, { cwd: cwd ?? ROOT, stderr: "inherit" });
  if (result.exitCode !== 0) {
    const out = new TextDecoder().decode(result.stdout);
    throw new Error(`Command failed (${result.exitCode}): ${cmd.join(" ")}\n${out}`);
  }
}

function exeName(platform: string): string {
  return platform === "win32" ? "knomit.exe" : "knomit";
}

function onnxLibName(platform: string): string {
  if (platform === "darwin") return "libonnxruntime.dylib";
  if (platform === "win32") return "onnxruntime.dll";
  return "libonnxruntime.so";
}

function sqliteLibName(platform: string): string {
  if (platform === "darwin") return "libsqlite3.dylib";
  if (platform === "win32") return "libsqlite3.dll";
  return "libsqlite3.so";
}

const DUGITE_VERSION = "v2.53.0";

const DUGITE_PLATFORM_MAP: Record<string, string> = {
  "darwin-arm64": "macOS-arm64",
  "linux-x64": "ubuntu-x64",
  "linux-arm64": "ubuntu-arm64",
  "win32-x64": "windows-x64",
};

async function compileBinary(target: Target, outDir: string) {
  const outFile = join(outDir, exeName(target.platform));
  log(`compiling binary for ${target.bunTarget}`);
  run([
    "bun", "build", "--compile",
    `--target=${target.bunTarget}`,
    join(ROOT, "src", "index.ts"),
    `--outfile=${outFile}`,
  ]);
}

async function downloadVecExtension(target: Target, libDir: string) {
  const url = getVecExtensionUrl(target.platform, target.arch);
  const filename = extensionFilename(target.platform);
  log(`downloading sqlite-vec from ${url}`);

  const resp = await fetch(url);
  if (!resp.ok) throw new Error(`Failed to download vec extension: HTTP ${resp.status}`);

  const tmpDir = join(libDir, "_vec_tmp");
  mkdirSync(tmpDir, { recursive: true });

  const tarPath = join(tmpDir, "vec.tar.gz");
  await Bun.write(tarPath, await resp.arrayBuffer());

  run(["tar", "xzf", tarPath, "-C", tmpDir]);

  // Find the extension file in extracted contents
  const glob = new Bun.Glob(`**/${filename}`);
  let found = false;
  for (const entry of glob.scanSync({ cwd: tmpDir })) {
    const src = join(tmpDir, entry);
    const dest = join(libDir, filename);
    cpSync(src, dest);
    found = true;
    break;
  }
  if (!found) throw new Error(`Could not find ${filename} in downloaded archive`);

  // Clean up
  rmSync(tmpDir, { recursive: true, force: true });
  log(`installed ${filename}`);
}

function copyOnnxRuntime(target: Target, libDir: string) {
  const onnxBase = join(ROOT, "src", "node_modules", "onnxruntime-node", "bin", "napi-v6");
  const platformDir = join(onnxBase, target.platform, target.arch);

  // Copy onnxruntime_binding.node
  const bindingPath = join(platformDir, "onnxruntime_binding.node");
  cpSync(bindingPath, join(libDir, "onnxruntime_binding.node"));
  log("copied onnxruntime_binding.node");

  // Copy the native onnxruntime library — find it by glob since names vary
  const libName = onnxLibName(target.platform);
  const glob = new Bun.Glob("*onnxruntime*");
  for (const entry of glob.scanSync({ cwd: platformDir })) {
    if (entry === "onnxruntime_binding.node") continue;
    const src = join(platformDir, entry);
    // Copy with canonical name for consistency
    cpSync(src, join(libDir, libName));
    log(`copied ${entry} as ${libName}`);
    break;
  }
}

function copySqliteLib(target: Target, libDir: string) {
  if (target.platform !== "darwin") {
    log(`skipping SQLite lib copy for ${target.platform} (not needed)`);
    return;
  }

  const homebrewPath = target.arch === "arm64"
    ? "/opt/homebrew/opt/sqlite/lib/libsqlite3.dylib"
    : "/usr/local/opt/sqlite/lib/libsqlite3.dylib";

  // Resolve symlinks to get the real file
  const result = Bun.spawnSync(["readlink", "-f", homebrewPath]);
  const realPath = new TextDecoder().decode(result.stdout).trim();
  const sourcePath = result.exitCode === 0 && realPath ? realPath : homebrewPath;

  const destName = sqliteLibName(target.platform);
  try {
    cpSync(sourcePath, join(libDir, destName));
    log(`copied ${basename(sourcePath)} as ${destName}`);
  } catch (err) {
    log(`WARNING: could not copy SQLite lib from ${sourcePath}: ${err}`);
    log("macOS builds may need Homebrew SQLite installed locally");
  }
}

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
  const platKey = DUGITE_PLATFORM_MAP[`${target.platform}-${target.arch}`];
  if (!platKey) throw new Error(`No dugite-native build for ${target.platform}-${target.arch}`);

  // Fetch release assets list to find exact filename (includes commit hash)
  const releaseUrl = `https://api.github.com/repos/desktop/dugite-native/releases/tags/${DUGITE_VERSION}`;
  log(`fetching dugite-native release info`);
  const releaseResp = await fetch(releaseUrl);
  if (!releaseResp.ok) throw new Error(`Failed to fetch release info: HTTP ${releaseResp.status}`);
  const release = await releaseResp.json() as { assets: Array<{ name: string; browser_download_url: string }> };

  // Match asset: dugite-native-<version>-<hash>-<platform>.tar.gz (not lzma)
  const asset = release.assets.find((a: { name: string }) =>
    a.name.includes(platKey) && a.name.endsWith(".tar.gz") && !a.name.includes("lzma")
  );
  if (!asset) throw new Error(`No dugite-native asset found for ${platKey}`);

  log(`downloading vendored git from ${asset.browser_download_url}`);
  const resp = await fetch(asset.browser_download_url);
  if (!resp.ok) throw new Error(`Failed to download git: HTTP ${resp.status}`);

  const gitDir = join(vendorDir, "git");
  mkdirSync(gitDir, { recursive: true });
  const tarPath = join(vendorDir, "git.tar.gz");
  await Bun.write(tarPath, await resp.arrayBuffer());

  // Extract — dugite-native tarballs have no top-level directory, extract into git/
  run(["tar", "xzf", tarPath, "-C", gitDir]);
  rmSync(tarPath);

  // Strip blacklisted components from libexec/git-core/
  const gitCoreDir = join(gitDir, "libexec", "git-core");
  const blacklistSet = new Set(DUGITE_BLACKLIST);
  for (const entry of new Bun.Glob("*").scanSync({ cwd: gitCoreDir })) {
    if (blacklistSet.has(entry) || [...blacklistSet].some((b) => entry.startsWith(b))) {
      try {
        rmSync(join(gitCoreDir, entry), { recursive: true, force: true });
        log(`stripped ${entry}`);
      } catch { /* ignore */ }
    }
  }

  // Strip non-essential directories
  for (const dir of ["share/gitweb", "share/perl5"]) {
    const fullPath = join(gitDir, dir);
    try {
      rmSync(fullPath, { recursive: true, force: true });
      log(`stripped ${dir}`);
    } catch { /* ignore */ }
  }

  log(`installed vendored git ${DUGITE_VERSION}`);
}

function createTarball(target: Target) {
  const slug = `${target.platform}-${target.arch}`;
  const tarName = `knomit-${slug}.tar.gz`;
  log(`creating ${tarName}`);
  run(["tar", "czf", join(DIST, tarName), "-C", join(DIST, slug), "knomit"]);
}

async function buildTarget(target: Target) {
  const slug = `${target.platform}-${target.arch}`;
  const outDir = join(DIST, slug, "knomit");
  const libDir = join(outDir, "lib");

  log(`\n========== Building ${slug} ==========`);

  // Clean and create directories
  rmSync(join(DIST, slug), { recursive: true, force: true });
  mkdirSync(libDir, { recursive: true });

  // Step 1: Compile binary
  await compileBinary(target, outDir);

  // Step 2: Download sqlite-vec extension
  await downloadVecExtension(target, libDir);

  // Step 3: Copy onnxruntime native files
  copyOnnxRuntime(target, libDir);

  // Step 4: Copy SQLite lib (macOS only)
  copySqliteLib(target, libDir);

  // Step 5: Download vendored git
  const vendorDir = join(outDir, "vendor");
  await downloadGit(target, vendorDir);

  // Step 6: Create tarball
  createTarball(target);

  log(`done: ${slug}`);
}

// --- Main ---

const platformArg = process.argv.find((_, i, arr) => arr[i - 1] === "--platform");

let targets: Target[];
if (platformArg) {
  const match = TARGETS.find(
    (t) => `${t.platform}-${t.arch}` === platformArg
  );
  if (!match) {
    console.error(`Unknown platform: ${platformArg}`);
    console.error(`Available: ${TARGETS.map((t) => `${t.platform}-${t.arch}`).join(", ")}`);
    process.exit(1);
  }
  targets = [match];
} else {
  targets = [...TARGETS];
}

mkdirSync(DIST, { recursive: true });

for (const target of targets) {
  await buildTarget(target);
}

log("\nAll builds complete!");
