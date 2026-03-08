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

/** Greedy longest-match-first WordPiece tokenization. */
export function wordPiece(word: string, vocab: Record<string, number>): number[] {
  const ids: number[] = [];
  let start = 0;

  while (start < word.length) {
    let end = word.length;
    let matched = false;

    while (start < end) {
      const substr = start === 0 ? word.slice(start, end) : `##${word.slice(start, end)}`;
      const id = vocab[substr];
      if (id != null) {
        ids.push(id);
        start = end;
        matched = true;
        break;
      }
      end--;
    }

    if (!matched) {
      return [100]; // [UNK]
    }
  }

  return ids;
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
    const maxLen = 512;

    // Normalize: lowercase, strip accents, collapse whitespace
    const normalized = text
      .toLowerCase()
      .normalize("NFD")
      .replace(/[\u0300-\u036f]/g, "")
      .replace(/\s+/g, " ")
      .trim();

    // Pre-tokenize: split on whitespace, then split each token on punctuation boundaries
    const preTokens: string[] = [];
    for (const word of normalized.split(" ")) {
      if (!word) continue;
      let current = "";
      for (const ch of word) {
        if (/[\p{P}\p{S}]/u.test(ch)) {
          if (current) preTokens.push(current);
          preTokens.push(ch);
          current = "";
        } else {
          current += ch;
        }
      }
      if (current) preTokens.push(current);
    }

    const ids: number[] = [101]; // [CLS]
    const mask: number[] = [1];

    for (const token of preTokens) {
      const pieces = wordPiece(token, vocab);
      for (const id of pieces) {
        if (ids.length >= maxLen - 1) break;
        ids.push(id);
        mask.push(1);
      }
      if (ids.length >= maxLen - 1) break;
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
