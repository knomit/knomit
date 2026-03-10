import { describe, it, expect } from "bun:test";
import { parseKnomitRef } from "./refs";

describe("parseKnomitRef", () => {
  it("parses local ref", () => {
    expect(parseKnomitRef("knomit:blob/abc1234/worlds/people/alice/likes-rock.md"))
      .toEqual({ path: "worlds/people/alice/likes-rock.md", commit: "abc1234" });
  });

  it("parses external ref", () => {
    expect(parseKnomitRef("knomit://github.com/org/repo/blob/def5678/src/file.ts"))
      .toEqual({ path: "src/file.ts", commit: "def5678", external: true });
  });

  it("returns null for plain URLs", () => {
    expect(parseKnomitRef("https://example.com")).toBeNull();
  });

  it("returns null for non-URI strings", () => {
    expect(parseKnomitRef("issue #42")).toBeNull();
  });

  it("handles paths with multiple slashes", () => {
    expect(parseKnomitRef("knomit:blob/aaa1111/worlds/a/b/c/d.md"))
      .toEqual({ path: "worlds/a/b/c/d.md", commit: "aaa1111" });
  });
});
