import { join, dirname } from "node:path";
import { existsSync, copyFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { log } from "./logger";
import { bundledLibDir } from "./paths";

function ensureOnnxLibInTmpdir(): void {
  // The compiled binary extracts native .node addons to os.tmpdir().
  // The .node addon uses @rpath which resolves to that same temp dir,
  // so we must place the onnxruntime dylib/so there.
  const libName = process.platform === "darwin"
    ? "libonnxruntime.1.24.3.dylib"
    : "libonnxruntime.so.1";
  const dest = join(tmpdir(), libName);

  if (existsSync(dest)) return;

  // Search for the library in known locations
  const candidates = [
    join(bundledLibDir(), libName),
    join(dirname(process.execPath), "node_modules", "onnxruntime-node", "bin", "napi-v6",
      process.platform, process.arch, libName),
    join(import.meta.dir, "node_modules", "onnxruntime-node", "bin", "napi-v6",
      process.platform, process.arch, libName),
  ];

  for (const src of candidates) {
    if (existsSync(src)) {
      try {
        copyFileSync(src, dest);
        log.info(`copied ${libName} to ${dest}`);
        return;
      } catch (err) {
        log.debug(`failed to copy ${libName} from ${src}: ${err}`);
      }
    }
  }

  log.warn(`could not find ${libName} to copy to tmpdir`);
}

export class Embedder {
  private ort: typeof import("onnxruntime-node") | null = null;
  private session: unknown = null;
  private tokenizer: unknown = null;

  async init(modelPath: string, tokenizerPath: string): Promise<void> {
    // Ensure onnxruntime dylib is in tmpdir where the .node addon expects it
    ensureOnnxLibInTmpdir();
    // Lazy import — only loaded when embeddings enabled
    this.ort = await import("onnxruntime-node");
    this.session = await this.ort.InferenceSession.create(modelPath);

    const tokenizerJson = await Bun.file(tokenizerPath).json();
    this.tokenizer = tokenizerJson;
    log.info("embedder initialized");
  }

  async embed(text: string): Promise<Float32Array> {
    if (!this.session || !this.ort) throw new Error("Embedder not initialized");

    const encoded = this.tokenize(text);

    const inputIds = new this.ort.Tensor("int64", BigInt64Array.from(encoded.ids.map(BigInt)), [1, encoded.ids.length]);
    const attentionMask = new this.ort.Tensor("int64", BigInt64Array.from(encoded.mask.map(BigInt)), [1, encoded.mask.length]);
    const tokenTypeIds = new this.ort.Tensor("int64", new BigInt64Array(encoded.ids.length), [1, encoded.ids.length]);

    const session = this.session as Awaited<ReturnType<typeof this.ort.InferenceSession.create>>;
    const output = await session.run({
      input_ids: inputIds,
      attention_mask: attentionMask,
      token_type_ids: tokenTypeIds,
    });

    // Mean pooling over token embeddings
    const lastHidden = output["last_hidden_state"] ?? output[Object.keys(output)[0]!]!;
    const data = lastHidden!.data as Float32Array;
    const seqLen = encoded.ids.length;
    const hiddenSize = data.length / seqLen;

    const pooled = new Float32Array(hiddenSize);
    for (let i = 0; i < seqLen; i++) {
      if (encoded.mask[i] === 1) {
        for (let j = 0; j < hiddenSize; j++) {
          pooled[j] += data[i * hiddenSize + j];
        }
      }
    }
    const maskSum = encoded.mask.reduce((a, b) => a + b, 0);
    for (let j = 0; j < hiddenSize; j++) {
      pooled[j] /= maskSum;
    }

    // L2 normalize
    const norm = Math.sqrt(pooled.reduce((a, b) => a + b * b, 0));
    for (let j = 0; j < hiddenSize; j++) {
      pooled[j] /= norm;
    }

    return pooled;
  }

  private tokenize(text: string): { ids: number[]; mask: number[] } {
    const tok = this.tokenizer as { model: { vocab: Record<string, number> } };
    const vocab = tok.model.vocab;

    const words = text.toLowerCase().split(/\s+/).filter(Boolean);
    const ids: number[] = [101]; // [CLS]
    const mask: number[] = [1];

    for (const word of words) {
      const id = vocab[word];
      if (id != null) {
        ids.push(id);
      } else {
        ids.push(100); // [UNK]
      }
      mask.push(1);
    }

    ids.push(102); // [SEP]
    mask.push(1);

    return { ids, mask };
  }

  close(): void {
    this.session = null;
    this.tokenizer = null;
  }
}
