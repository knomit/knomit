import { test, expect } from "bun:test";
import { resolveProvider } from "../src/llm";

test("resolveProvider accepts claude-cli", () => {
  expect(resolveProvider("anything", "claude-cli")).toBe("claude-cli");
});

test("resolveProvider accepts gemini-cli", () => {
  expect(resolveProvider("anything", "gemini-cli")).toBe("gemini-cli");
});

test("resolveProvider rejects invalid provider", () => {
  expect(() => resolveProvider("anything", "invalid")).toThrow(/Invalid provider/);
});

test("resolveProvider infers anthropic from claude model", () => {
  expect(resolveProvider("claude-sonnet-4-6")).toBe("anthropic");
});

test("resolveProvider infers gemini from gemini model", () => {
  expect(resolveProvider("gemini-2.0-flash")).toBe("gemini");
});

test("resolveProvider infers bedrock from anthropic. model", () => {
  expect(resolveProvider("anthropic.claude-3-5-sonnet")).toBe("bedrock");
});

test("resolveProvider throws for unknown model without explicit provider", () => {
  expect(() => resolveProvider("unknown-model")).toThrow(/Cannot infer provider/);
});
