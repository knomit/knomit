import { describe, it, expect } from "bun:test";
import { parseFact, serializeFact, mergeFrontmatter } from "./facts";

describe("parseFact", () => {
  it("parses YAML frontmatter and markdown body", () => {
    const raw = `---
domain: [personal, music]
confidence: 0.85
sources: 3
entities: [alice, rock_music]
refs:
  - episodic://spotify-2024
---
# Alice likes rock music

She has a strong preference for rock.`;

    const result = parseFact(raw);
    expect(result.frontmatter.domain).toEqual(["personal", "music"]);
    expect(result.frontmatter.confidence).toBe(0.85);
    expect(result.frontmatter.sources).toBe(3);
    expect(result.frontmatter.entities).toEqual(["alice", "rock_music"]);
    expect(result.frontmatter.refs).toEqual(["episodic://spotify-2024"]);
    expect(result.title).toBe("Alice likes rock music");
    expect(result.body).toBe("She has a strong preference for rock.");
  });

  it("handles missing refs as empty array", () => {
    const raw = `---
domain: [test]
confidence: 0.5
sources: 1
entities: [foo]
---
# Title

Body text.`;

    const result = parseFact(raw);
    expect(result.frontmatter.refs).toEqual([]);
    expect(result.title).toBe("Title");
  });

  it("throws on invalid YAML", () => {
    const raw = `---
domain: [unclosed
---
# Title

Body.`;

    expect(() => parseFact(raw)).toThrow();
  });
});

describe("serializeFact", () => {
  it("produces valid markdown with frontmatter", () => {
    const frontmatter = {
      domain: ["personal"],
      confidence: 0.9,
      sources: 2,
      entities: ["alice"],
      refs: ["abc123"],
    };
    const result = serializeFact(frontmatter, "Alice is kind", "She is very kind.");

    expect(result).toContain("---");
    expect(result).toContain("# Alice is kind");
    expect(result).toContain("She is very kind.");

    // Round-trip: parse what we serialized
    const parsed = parseFact(result);
    expect(parsed.frontmatter).toEqual(frontmatter);
    expect(parsed.title).toBe("Alice is kind");
    expect(parsed.body).toBe("She is very kind.");
  });
});

describe("mergeFrontmatter", () => {
  it("merges partial updates into existing frontmatter", () => {
    const existing = {
      domain: ["personal"],
      confidence: 0.5,
      sources: 1,
      entities: ["alice"],
      refs: ["abc"],
    };
    const updates = { confidence: 0.8, sources: 3 };
    const result = mergeFrontmatter(existing, updates);

    expect(result.confidence).toBe(0.8);
    expect(result.sources).toBe(3);
    expect(result.domain).toEqual(["personal"]);
    expect(result.entities).toEqual(["alice"]);
    expect(result.refs).toEqual(["abc"]);
  });

  it("appends refs instead of replacing", () => {
    const existing = {
      domain: ["personal"],
      confidence: 0.5,
      sources: 1,
      entities: ["alice"],
      refs: ["abc"],
    };
    const updates = { refs: ["def", "ghi"] };
    const result = mergeFrontmatter(existing, updates);
    expect(result.refs).toEqual(["abc", "def", "ghi"]);
  });

  it("replaces domain and entities entirely", () => {
    const existing = {
      domain: ["personal"],
      confidence: 0.5,
      sources: 1,
      entities: ["alice"],
      refs: [],
    };
    const updates = { domain: ["work", "music"], entities: ["bob"] };
    const result = mergeFrontmatter(existing, updates);
    expect(result.domain).toEqual(["work", "music"]);
    expect(result.entities).toEqual(["bob"]);
  });
});
