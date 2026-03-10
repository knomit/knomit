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

  it("different commits with same file count produce different items", () => {
    // Regression: onItemsChanged effect used selectableItems.length as dependency,
    // so scrolling between commits with the same number of changed files left
    // rightItemsRef stale, causing navigation to the wrong file.
    const itemsA = buildChangedFileItems(
      { added: ["kubernetes-platform-standardization.md"], modified: [], deleted: [] },
      "worlds",
    );
    const itemsB = buildChangedFileItems(
      { added: ["k8s-networking.md"], modified: [], deleted: [] },
      "worlds",
    );
    expect(itemsA).toHaveLength(1);
    expect(itemsB).toHaveLength(1);
    expect(itemsA[0].path).not.toBe(itemsB[0].path);
    expect(itemsA[0].label).not.toBe(itemsB[0].label);
  });
});
