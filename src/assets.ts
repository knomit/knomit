import { exists, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { log } from "./logger";
import { resolveLib } from "./paths";

const SQLITE_VEC_VERSION = "v0.1.6";
const ONNX_MODEL_REPO = "xenova/all-MiniLM-L6-v2";

const PLATFORM_MAP: Record<string, string> = {
  "darwin-arm64": "macos-aarch64",
  "darwin-x64": "macos-x86_64",
  "linux-x64": "linux-x86_64",
  "linux-arm64": "linux-aarch64",
  "win32-x64": "windows-x86_64",
};

export function extensionFilename(platform: string): string {
  if (platform === "darwin") return "vec0.dylib";
  if (platform === "win32") return "vec0.dll";
  return "vec0.so";
}

export function getVecExtensionUrl(platform: string, arch: string): string {
  const key = `${platform}-${arch}`;
  const target = PLATFORM_MAP[key];
  if (!target) throw new Error(`Unsupported platform: ${key}`);
  const version = SQLITE_VEC_VERSION.replace(/^v/, "");
  return `https://github.com/asg017/sqlite-vec/releases/download/${SQLITE_VEC_VERSION}/sqlite-vec-${version}-loadable-${target}.tar.gz`;
}

export function getOnnxModelUrl(): { model: string; tokenizer: string } {
  const base = `https://huggingface.co/${ONNX_MODEL_REPO}/resolve/main`;
  return {
    model: `${base}/onnx/model_quantized.onnx`,
    tokenizer: `${base}/tokenizer.json`,
  };
}

export async function ensureVecExtension(cacheDir: string): Promise<string | null> {
  const platform = process.platform;
  const arch = process.arch;
  const filename = extensionFilename(platform);

  // 1. Check bundled lib/ directory
  const bundled = await resolveLib(filename);
  if (bundled) return bundled;

  // 2. Check cache directory (existing behavior)
  const extDir = join(cacheDir, "extensions");
  const extPath = join(extDir, filename);

  if (await exists(extPath)) return extPath;

  try {
    await mkdir(extDir, { recursive: true });
    const url = getVecExtensionUrl(platform, arch);
    log.info(`downloading sqlite-vec from ${url}`);

    const response = await fetch(url);
    if (!response.ok) throw new Error(`HTTP ${response.status}`);

    // sqlite-vec releases are .tar.gz — extract the .dylib/.so file
    const tarGz = await response.arrayBuffer();
    const tmpTar = join(extDir, "vec.tar.gz");
    await writeFile(tmpTar, Buffer.from(tarGz));

    const proc = Bun.spawnSync(["tar", "xzf", tmpTar, "-C", extDir]);
    if (proc.exitCode !== 0) throw new Error("tar extract failed");

    // The extracted file may be in a subdirectory — find it
    const findProc = Bun.spawnSync(["find", extDir, "-name", filename, "-type", "f"]);
    const found = new TextDecoder().decode(findProc.stdout).trim().split("\n")[0];
    if (found && found !== extPath) {
      await Bun.write(extPath, Bun.file(found));
    }

    log.info(`sqlite-vec installed at ${extPath}`);
    return extPath;
  } catch (err) {
    log.warn(`failed to download sqlite-vec: ${err}`);
    return null;
  }
}

export async function ensureOnnxModel(cacheDir: string): Promise<{ model: string; tokenizer: string } | null> {
  const modelDir = join(cacheDir, "models", "all-MiniLM-L6-v2");
  const modelPath = join(modelDir, "model_quantized.onnx");
  const tokenizerPath = join(modelDir, "tokenizer.json");

  if ((await exists(modelPath)) && (await exists(tokenizerPath))) {
    return { model: modelPath, tokenizer: tokenizerPath };
  }

  try {
    await mkdir(modelDir, { recursive: true });
    const urls = getOnnxModelUrl();

    log.info("downloading ONNX model...");
    const [modelResp, tokResp] = await Promise.all([
      fetch(urls.model),
      fetch(urls.tokenizer),
    ]);
    if (!modelResp.ok) throw new Error(`Model download failed: HTTP ${modelResp.status}`);
    if (!tokResp.ok) throw new Error(`Tokenizer download failed: HTTP ${tokResp.status}`);

    await Promise.all([
      writeFile(modelPath, Buffer.from(await modelResp.arrayBuffer())),
      writeFile(tokenizerPath, Buffer.from(await tokResp.arrayBuffer())),
    ]);

    log.info(`ONNX model installed at ${modelDir}`);
    return { model: modelPath, tokenizer: tokenizerPath };
  } catch (err) {
    log.warn(`failed to download ONNX model: ${err}`);
    return null;
  }
}
