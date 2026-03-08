import { describe, it, expect } from "bun:test";
import { Embedder, wordPiece } from "./embeddings";

// Minimal vocab for testing WordPiece — mirrors BERT-style tokenizer
const vocab: Record<string, number> = {
  hello: 7592,
  world: 2088,
  java: 9764,
  "##script": 14167,
  run: 2448,
  "##time": 7292,
  "##s": 1055,
  the: 1996,
  ".": 1012,
  "-": 1011,
  "@": 1030,
  "[UNK]": 100,
};

describe("wordPiece", () => {
  it("matches whole words", () => {
    expect(wordPiece("hello", vocab)).toEqual([7592]);
    expect(wordPiece("world", vocab)).toEqual([2088]);
  });

  it("splits compound words into subwords", () => {
    expect(wordPiece("javascript", vocab)).toEqual([9764, 14167]);
  });

  it("handles multiple continuation pieces", () => {
    expect(wordPiece("runtimes", vocab)).toEqual([2448, 7292, 1055]);
  });

  it("returns [UNK] for completely unknown words", () => {
    expect(wordPiece("zzzzz", vocab)).toEqual([100]);
  });

  it("returns [UNK] if any piece fails", () => {
    // "jav" is not in vocab and can't be decomposed
    expect(wordPiece("jav", vocab)).toEqual([100]);
  });

  it("handles single-char tokens", () => {
    expect(wordPiece(".", vocab)).toEqual([1012]);
    expect(wordPiece("@", vocab)).toEqual([1030]);
  });
});

describe("Embedder", () => {
  it("exports the Embedder class", () => {
    expect(Embedder).toBeDefined();
  });

  // Integration test — only runs if model is downloaded
  // Skipped in CI unless KNOMIT_EMBEDDINGS=1
  it.skipIf(!process.env.KNOMIT_EMBEDDINGS)("generates 384-dim embeddings", async () => {
    const embedder = new Embedder();
    await embedder.init(
      process.env.KNOMIT_TEST_MODEL_PATH!,
      process.env.KNOMIT_TEST_TOKENIZER_PATH!
    );
    const vec = await embedder.embed("hello world");
    expect(vec.length).toBe(384);
    // Values should be normalized floats
    expect(Math.abs(vec.reduce((a, b) => a + b * b, 0) - 1.0)).toBeLessThan(0.01);
    embedder.close();
  });
});
