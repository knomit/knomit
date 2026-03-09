import { describe, it, expect } from "bun:test";
import { parseRecipe, type Recipe } from "./recipe";

describe("parseRecipe", () => {
  it("parses a valid recipe", () => {
    const yaml = `
name: cve-review
prompt: "Review CVEs for staleness"
scope:
  domain: [security]
  entities: [acme]
  search: ["patch"]
  path: worlds/security/
auto_merge: false
steps:
  - mode: prune
    model: gemini-2.0-flash
    prompt: "Find stale CVEs"
  - mode: distill
    model: claude-sonnet-4-6
    prompt: "Find patterns"
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.name).toBe("cve-review");
    expect(recipe.steps).toHaveLength(2);
    expect(recipe.steps[0].mode).toBe("prune");
    expect(recipe.steps[0].model).toBe("gemini-2.0-flash");
    expect(recipe.steps[1].mode).toBe("distill");
    expect(recipe.scope.domain).toEqual(["security"]);
    expect(recipe.scope.entities).toEqual(["acme"]);
    expect(recipe.scope.search).toEqual(["patch"]);
    expect(recipe.scope.path).toBe("worlds/security/");
    expect(recipe.auto_merge).toBe(false);
  });

  it("defaults auto_merge to false", () => {
    const yaml = `
name: test
prompt: ""
steps:
  - mode: prune
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.auto_merge).toBe(false);
  });

  it("throws on missing name", () => {
    const yaml = `
steps:
  - mode: prune
`;
    expect(() => parseRecipe(yaml)).toThrow();
  });

  it("throws on invalid mode", () => {
    const yaml = `
name: test
steps:
  - mode: invalid
`;
    expect(() => parseRecipe(yaml)).toThrow();
  });

  it("defaults empty scope fields", () => {
    const yaml = `
name: test
scope:
  domain: []
steps:
  - mode: prune
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.scope!.domain).toEqual([]);
    expect(recipe.scope!.entities).toEqual([]);
    expect(recipe.scope!.search).toEqual([]);
    expect(recipe.scope!.path).toBe("");
  });

  it("allows omitting scope entirely for auto-discovery", () => {
    const yaml = `
name: auto-test
steps:
  - mode: prune
`;
    const recipe = parseRecipe(yaml);
    expect(recipe.scope).toBeUndefined();
  });
});
