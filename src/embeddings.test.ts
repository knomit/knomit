import { describe, it, expect } from "bun:test";
import { Embedder } from "./embeddings";

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
