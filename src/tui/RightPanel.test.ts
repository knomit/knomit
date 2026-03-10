import { describe, it, expect } from "bun:test";
import { buildChangedFileItems } from "./RightPanel";

describe("buildChangedFileItems", () => {
  it("builds items in order: added, modified, deleted", () => {
    const items = buildChangedFileItems(
      { added: ["a.md"], modified: ["b.md"], deleted: ["c.md"] },
      "worlds/people",
    );
    expect(items).toEqual([
      { type: "changed-file", label: "a.md", path: "worlds/people/a.md", changeStatus: "added" },
      { type: "changed-file", label: "b.md", path: "worlds/people/b.md", changeStatus: "modified" },
      { type: "changed-file", label: "c.md", path: "worlds/people/c.md", changeStatus: "deleted" },
    ]);
  });

  it("returns empty array for undefined", () => {
    expect(buildChangedFileItems(undefined, "worlds")).toEqual([]);
  });

  it("returns empty array when no changes", () => {
    expect(buildChangedFileItems({ added: [], modified: [], deleted: [] }, "worlds")).toEqual([]);
  });
});
