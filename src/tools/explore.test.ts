import { describe, it, expect, beforeEach, afterEach } from "bun:test";
import { GitRepo } from "../git";
import { learnHandler } from "./learn";
import { exploreHandler } from "./explore";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

let testDir: string;
let repo: GitRepo;

beforeEach(async () => {
  testDir = await mkdtemp(join(tmpdir(), "knomit-test-"));
  repo = new GitRepo(join(testDir, "repo"), "test-machine");
  await repo.init();

  await learnHandler(repo, {
    moment_name: "setup",
    facts: [
      {
        path: "worlds/earth.md",
        domain: ["geography"],
        confidence: 0.99,
        sources: 1,
        entities: ["earth"],
        title: "Earth",
        body: "The third planet.",
      },
      {
        path: "worlds/earth/uk.md",
        domain: ["geography"],
        confidence: 0.99,
        sources: 1,
        entities: ["uk"],
        title: "United Kingdom",
        body: "Island nation in Europe.",
      },
      {
        path: "worlds/earth/uk/london.md",
        domain: ["geography", "urban"],
        confidence: 0.99,
        sources: 1,
        entities: ["london", "uk"],
        title: "London",
        body: "Capital city of the UK.",
      },
      {
        path: "worlds/earth/uk/london/london-rains.md",
        domain: ["geography", "weather"],
        confidence: 0.8,
        sources: 2,
        entities: ["london", "rain"],
        title: "London rains a lot",
        body: "Frequent rainfall in London.",
      },
      {
        path: "worlds/earth/uk/uk-drives-left.md",
        domain: ["geography", "transport"],
        confidence: 0.99,
        sources: 5,
        entities: ["uk", "driving"],
        title: "UK drives on the left",
        body: "Vehicles drive on the left side.",
      },
    ],
  });
});

afterEach(async () => {
  await rm(testDir, { recursive: true, force: true });
});

describe("knomit_explore", () => {
  it("lists root worlds/ contents", async () => {
    const result = await exploreHandler(repo, {});

    const names = result.children.map(c => c.name);
    expect(names).toContain("earth");
    expect(names).toContain("earth.md");
  });

  it("shows manifest for a world", async () => {
    const result = await exploreHandler(repo, { path: "worlds/earth/uk/london" });

    expect(result.manifest).not.toBeNull();
    expect(result.manifest!.file).toBe("worlds/earth/uk/london.md");

    const names = result.children.map(c => c.name);
    expect(names).toContain("london-rains.md");
  });

  it("shows inherited facts from parent levels", async () => {
    const result = await exploreHandler(repo, { path: "worlds/earth/uk/london" });

    const inheritedFiles = result.inherited_facts.map(f => f.file);
    expect(inheritedFiles).toContain("worlds/earth/uk/uk-drives-left.md");

    const drivesLeft = result.inherited_facts.find(f => f.file === "worlds/earth/uk/uk-drives-left.md");
    expect(drivesLeft?.from_level).toBe("worlds/earth/uk");
  });

  it("identifies worlds vs facts in children", async () => {
    const result = await exploreHandler(repo, { path: "worlds/earth/uk" });

    const london = result.children.find(c => c.name === "london");
    expect(london?.type).toBe("world");

    const drivesLeft = result.children.find(c => c.name === "uk-drives-left.md");
    expect(drivesLeft?.type).toBe("fact");
  });
});
