/**
 * E2E tests for recipe parsing and MCP profile instructions.
 */
import { describe, it, expect } from "bun:test";
import { parseRecipe } from "../recipe";
import { getInstructions, MCP_PROFILES } from "../instructions";

// ---------------------------------------------------------------------------
// Recipe parsing
// ---------------------------------------------------------------------------

describe("recipe parsing", () => {
  it("parses a minimal recipe", () => {
    const recipe = parseRecipe(`
name: simple
steps:
  - mode: prune
`);
    expect(recipe.name).toBe("simple");
    expect(recipe.steps).toHaveLength(1);
    expect(recipe.steps[0].mode).toBe("prune");
    expect(recipe.auto_merge).toBe(false);
    expect(recipe.prompt).toBe("");
    expect(recipe.scope).toBeUndefined(); // auto-discovery mode
  });

  it("parses a fully specified recipe", () => {
    const recipe = parseRecipe(`
name: security-review
prompt: "Focus on vulnerability patterns"
scope:
  domain: [security, compliance]
  entities: [libfoo, libbar]
  search: [buffer overflow, injection]
  path: know/security
auto_merge: true
steps:
  - mode: prune
    model: gemini-2.0-flash
    prompt: "Identify patched CVEs"
  - mode: distill
    model: claude-sonnet-4-6
    prompt: "Find vendor patterns"
`);
    expect(recipe.name).toBe("security-review");
    expect(recipe.prompt).toBe("Focus on vulnerability patterns");
    expect(recipe.scope!.domain).toEqual(["security", "compliance"]);
    expect(recipe.scope!.entities).toEqual(["libfoo", "libbar"]);
    expect(recipe.scope!.search).toEqual(["buffer overflow", "injection"]);
    expect(recipe.scope!.path).toBe("know/security");
    expect(recipe.auto_merge).toBe(true);
    expect(recipe.steps).toHaveLength(2);
    expect(recipe.steps[0].mode).toBe("prune");
    expect(recipe.steps[0].model).toBe("gemini-2.0-flash");
    expect(recipe.steps[1].mode).toBe("distill");
    expect(recipe.steps[1].model).toBe("claude-sonnet-4-6");
  });

  it("rejects recipe without name", () => {
    expect(() => parseRecipe(`
steps:
  - mode: prune
`)).toThrow();
  });

  it("rejects recipe without steps", () => {
    expect(() => parseRecipe(`
name: no-steps
`)).toThrow();
  });

  it("rejects empty steps array", () => {
    expect(() => parseRecipe(`
name: empty-steps
steps: []
`)).toThrow();
  });

  it("rejects invalid step mode", () => {
    expect(() => parseRecipe(`
name: bad-mode
steps:
  - mode: invalid
`)).toThrow();
  });

  it("defaults optional scope fields", () => {
    const recipe = parseRecipe(`
name: partial-scope
scope:
  domain: [testing]
steps:
  - mode: prune
`);
    expect(recipe.scope!.domain).toEqual(["testing"]);
    expect(recipe.scope!.entities).toEqual([]);
    expect(recipe.scope!.search).toEqual([]);
    expect(recipe.scope!.path).toBe("");
  });

  it("treats missing scope as auto-discovery", () => {
    const recipe = parseRecipe(`
name: auto-discover
steps:
  - mode: prune
  - mode: distill
`);
    expect(recipe.scope).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Instructions / profiles
// ---------------------------------------------------------------------------

describe("MCP instructions", () => {
  it("generates code profile instructions", () => {
    const instructions = getInstructions("code");
    expect(instructions).toContain("Knomit");
    expect(instructions).toContain("CODE EDITOR INTEGRATION");
    expect(instructions).toContain("knomit_learn");
    expect(instructions).not.toContain("CONVERSATIONAL INTEGRATION");
  });

  it("generates chat profile instructions", () => {
    const instructions = getInstructions("chat");
    expect(instructions).toContain("CONVERSATIONAL INTEGRATION");
    expect(instructions).not.toContain("CODE EDITOR INTEGRATION");
  });

  it("generates generic profile instructions", () => {
    const instructions = getInstructions("generic");
    expect(instructions).not.toContain("CODE EDITOR INTEGRATION");
    expect(instructions).not.toContain("CONVERSATIONAL INTEGRATION");
    expect(instructions).toContain("session start");
  });

  it("all profiles include base instructions", () => {
    for (const profile of MCP_PROFILES) {
      const instructions = getInstructions(profile);
      expect(instructions).toContain("knomit_learn");
      expect(instructions).toContain("knomit_why");
      expect(instructions).toContain("knomit_update");
      expect(instructions).toContain("Confidence");
    }
  });

  it("defaults to code profile", () => {
    const defaultInstructions = getInstructions();
    const codeInstructions = getInstructions("code");
    expect(defaultInstructions).toBe(codeInstructions);
  });
});
