import { describe, it, expect } from "bun:test";
import { createAdapter, resolveProvider } from "./llm";

describe("resolveProvider", () => {
  it("resolves provider from model name", () => {
    expect(resolveProvider("claude-sonnet-4-6")).toBe("anthropic");
    expect(resolveProvider("gemini-2.0-flash")).toBe("gemini");
    expect(resolveProvider("us.anthropic.claude-sonnet-4-6-v1")).toBe("bedrock");
  });

  it("returns explicit provider when given", () => {
    expect(resolveProvider("my-model", "anthropic")).toBe("anthropic");
  });

  it("throws on unknown model without provider", () => {
    expect(() => resolveProvider("unknown-model")).toThrow();
  });
});

describe("createAdapter", () => {
  it("creates an anthropic adapter", () => {
    const adapter = createAdapter({
      provider: "anthropic",
      model: "claude-sonnet-4-6",
      apiKey: "sk-test",
    });
    expect(adapter).toBeDefined();
    expect(typeof adapter.complete).toBe("function");
  });

  it("creates a gemini adapter", () => {
    const adapter = createAdapter({
      provider: "gemini",
      model: "gemini-2.0-flash",
      apiKey: "AI-test",
    });
    expect(adapter).toBeDefined();
  });

  it("creates a bedrock adapter", () => {
    const adapter = createAdapter({
      provider: "bedrock",
      model: "us.anthropic.claude-sonnet-4-6-v1",
      region: "us-east-1",
      accessKeyId: "AKIA-test",
      secretAccessKey: "secret-test",
    });
    expect(adapter).toBeDefined();
  });

  it("throws without api key for anthropic", () => {
    const savedKey = process.env.ANTHROPIC_API_KEY;
    delete process.env.ANTHROPIC_API_KEY;
    try {
      expect(() =>
        createAdapter({ provider: "anthropic", model: "claude-sonnet-4-6" })
      ).toThrow();
    } finally {
      if (savedKey !== undefined) process.env.ANTHROPIC_API_KEY = savedKey;
    }
  });
});
