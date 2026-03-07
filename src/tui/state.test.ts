import { test, expect } from "bun:test";
import { initialState, reducer, type AppState, type Action } from "./state";

test("initial state starts at worlds root with breadcrumb selected", () => {
  expect(initialState.currentPath).toBe("worlds");
  expect(initialState.selectedIndex).toBe(0);
  expect(initialState.breadcrumbSelected).toBe(true);
  expect(initialState.focusZone).toBe("left");
  expect(initialState.rightPanelMode).toBe("summary");
  expect(initialState.searchActive).toBe(false);
  expect(initialState.statsPath).toBe("worlds");
});

test("SET_CHILDREN populates left panel items with breadcrumb selected", () => {
  const s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [
      { name: "physics", type: "world", summary: "Physics stuff" },
      { name: "readme.md", type: "fact", summary: "A fact" },
    ],
  });
  expect(s.children.length).toBe(2);
  expect(s.selectedIndex).toBe(0);
  expect(s.breadcrumbSelected).toBe(true);
  expect(s.statsPath).toBe("worlds");
});

test("NAVIGATE_DOWN from breadcrumb moves to first item", () => {
  let s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [
      { name: "a", type: "world" },
      { name: "b", type: "world" },
    ],
  });
  expect(s.breadcrumbSelected).toBe(true);
  s = reducer(s, { type: "NAVIGATE_DOWN" });
  expect(s.breadcrumbSelected).toBe(false);
  expect(s.selectedIndex).toBe(0);
  expect(s.statsPath).toBe("worlds/a");
});

test("NAVIGATE_DOWN increments selectedIndex within bounds", () => {
  let s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [
      { name: "a", type: "world" },
      { name: "b", type: "world" },
    ],
  });
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // breadcrumb -> index 0
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // index 0 -> index 1
  expect(s.selectedIndex).toBe(1);
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // stays at 1
  expect(s.selectedIndex).toBe(1);
});

test("NAVIGATE_UP from first item returns to breadcrumb", () => {
  let s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [
      { name: "a", type: "world" },
      { name: "b", type: "world" },
    ],
  });
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // breadcrumb -> index 0
  expect(s.breadcrumbSelected).toBe(false);
  s = reducer(s, { type: "NAVIGATE_UP" }); // index 0 -> breadcrumb
  expect(s.breadcrumbSelected).toBe(true);
  expect(s.statsPath).toBe("worlds");
  expect(s.rightPanelMode).toBe("summary");
});

test("NAVIGATE_UP at breadcrumb stays at breadcrumb", () => {
  const s = reducer(initialState, { type: "NAVIGATE_UP" });
  expect(s.breadcrumbSelected).toBe(true);
});

test("OPEN_ITEM on world updates currentPath and resets to breadcrumb", () => {
  let s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [{ name: "physics", type: "world" }],
  });
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // breadcrumb -> index 0
  s = reducer(s, { type: "OPEN_ITEM" });
  expect(s.currentPath).toBe("worlds/physics");
  expect(s.selectedIndex).toBe(0);
  expect(s.breadcrumbSelected).toBe(true);
  expect(s.currentFact).toBeNull();
  expect(s.rightPanelMode).toBe("summary");
  expect(s.statsPath).toBe("worlds/physics");
});

test("OPEN_ITEM on fact sets currentFact and switches right panel", () => {
  let s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [{ name: "gravity.md", type: "fact", summary: "Gravity" }],
  });
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // breadcrumb -> index 0
  s = reducer(s, { type: "OPEN_ITEM" });
  expect(s.currentFact).toBe("worlds/gravity.md");
  expect(s.rightPanelMode).toBe("fact");
});

test("OPEN_ITEM on breadcrumb does nothing (use GO_UP or left arrow)", () => {
  const s = reducer(initialState, { type: "OPEN_ITEM" });
  expect(s.currentPath).toBe("worlds");
});

test("GO_UP navigates to parent directory", () => {
  let s: AppState = { ...initialState, currentPath: "worlds/physics/mechanics", statsPath: "worlds/physics/mechanics" };
  s = reducer(s, { type: "GO_UP" });
  expect(s.currentPath).toBe("worlds/physics");
  expect(s.selectedIndex).toBe(0);
  expect(s.breadcrumbSelected).toBe(true);
  expect(s.currentFact).toBeNull();
  expect(s.statsPath).toBe("worlds/physics");
});

test("GO_UP at root stays at root", () => {
  const s = reducer(initialState, { type: "GO_UP" });
  expect(s.currentPath).toBe("worlds");
});

test("SET_FOCUS changes focus zone", () => {
  const s = reducer(initialState, { type: "SET_FOCUS", zone: "command" });
  expect(s.focusZone).toBe("command");
});

test("TOGGLE_HISTORY switches between fact and history modes", () => {
  let s: AppState = { ...initialState, rightPanelMode: "fact", currentFact: "worlds/x.md" };
  s = reducer(s, { type: "TOGGLE_HISTORY" });
  expect(s.rightPanelMode).toBe("history");
  s = reducer(s, { type: "TOGGLE_HISTORY" });
  expect(s.rightPanelMode).toBe("fact");
});

test("TOGGLE_HISTORY works without a fact selected (shows path history)", () => {
  let s = reducer(initialState, { type: "TOGGLE_HISTORY" });
  expect(s.rightPanelMode).toBe("history");
  s = reducer(s, { type: "TOGGLE_HISTORY" });
  expect(s.rightPanelMode).toBe("summary");
});

test("SET_SEARCH_RESULTS activates search mode and selects first fact", () => {
  const s = reducer(initialState, {
    type: "SET_SEARCH_RESULTS",
    results: [{ file: "worlds/x.md", title: "X", body: "body" }],
  });
  expect(s.searchActive).toBe(true);
  expect(s.searchResults.length).toBe(1);
  expect(s.selectedIndex).toBe(0);
  expect(s.currentFact).toBe("worlds/x.md");
  expect(s.rightPanelMode).toBe("fact");
});

test("NAVIGATE_DOWN in search auto-selects fact", () => {
  let s = reducer(initialState, {
    type: "SET_SEARCH_RESULTS",
    results: [
      { file: "worlds/a.md", title: "A", body: "a" },
      { file: "worlds/b.md", title: "B", body: "b" },
    ],
  });
  expect(s.currentFact).toBe("worlds/a.md");
  s = reducer(s, { type: "NAVIGATE_DOWN" });
  expect(s.selectedIndex).toBe(1);
  expect(s.currentFact).toBe("worlds/b.md");
  expect(s.rightPanelMode).toBe("fact");
});

test("NAVIGATE_DOWN in explorer auto-selects facts but shows stats for worlds", () => {
  let s = reducer(initialState, {
    type: "SET_CHILDREN",
    children: [
      { name: "physics", type: "world" },
      { name: "note.md", type: "fact" },
    ],
  });
  expect(s.currentFact).toBeNull();
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // breadcrumb -> index 0 (world)
  expect(s.selectedIndex).toBe(0);
  expect(s.currentFact).toBeNull();
  expect(s.statsPath).toBe("worlds/physics");
  expect(s.rightPanelMode).toBe("summary");
  s = reducer(s, { type: "NAVIGATE_DOWN" }); // index 0 -> index 1 (fact)
  expect(s.selectedIndex).toBe(1);
  expect(s.currentFact).toBe("worlds/note.md");
  expect(s.rightPanelMode).toBe("fact");
});

test("NAVIGATE_TO_PATH navigates to specific path", () => {
  let s: AppState = { ...initialState, currentPath: "worlds/a/b/c", statsPath: "worlds/a/b/c" };
  s = reducer(s, { type: "NAVIGATE_TO_PATH", path: "worlds/a" });
  expect(s.currentPath).toBe("worlds/a");
  expect(s.breadcrumbSelected).toBe(true);
  expect(s.statsPath).toBe("worlds/a");
  expect(s.selectedIndex).toBe(0);
});

test("CLEAR_SEARCH returns to explorer mode", () => {
  let s = reducer(initialState, {
    type: "SET_SEARCH_RESULTS",
    results: [{ file: "worlds/x.md", title: "X", body: "body" }],
  });
  s = reducer(s, { type: "CLEAR_SEARCH" });
  expect(s.searchActive).toBe(false);
  expect(s.searchResults).toEqual([]);
});
