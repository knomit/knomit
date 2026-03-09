/**
 * E2E tests for TUI state management (reducer).
 *
 * Tests navigation, search state transitions, history mode,
 * focus management, and edge cases in the reducer.
 */
import { describe, it, expect } from "bun:test";
import { reducer, initialState, type AppState, type ChildItem, type Action } from "../tui/state";
import type { LogEntry } from "../git";

/** Apply a sequence of actions and return the final state. */
function applyActions(state: AppState, actions: Action[]): AppState {
  return actions.reduce((s, a) => reducer(s, a), state);
}

/** Create state with children loaded. */
function stateWithChildren(children: ChildItem[]): AppState {
  return reducer(initialState, { type: "SET_CHILDREN", children });
}

// ---------------------------------------------------------------------------
// Initial state
// ---------------------------------------------------------------------------

describe("TUI initial state", () => {
  it("starts at worlds root with breadcrumb selected", () => {
    expect(initialState.currentPath).toBe("worlds");
    expect(initialState.breadcrumbSelected).toBe(true);
    expect(initialState.selectedIndex).toBe(0);
    expect(initialState.currentFact).toBeNull();
    expect(initialState.rightPanelMode).toBe("summary");
    expect(initialState.focusZone).toBe("left");
    expect(initialState.searchActive).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Navigation
// ---------------------------------------------------------------------------

describe("TUI navigation", () => {
  const children: ChildItem[] = [
    { name: "projects", type: "world" },
    { name: "people", type: "world" },
    { name: "note.md", type: "fact", summary: "A note" },
  ];

  it("NAVIGATE_DOWN from breadcrumb selects first child", () => {
    const state = stateWithChildren(children);
    expect(state.breadcrumbSelected).toBe(true);

    const next = reducer(state, { type: "NAVIGATE_DOWN" });
    expect(next.breadcrumbSelected).toBe(false);
    expect(next.selectedIndex).toBe(0);
  });

  it("NAVIGATE_DOWN moves through children", () => {
    let state = stateWithChildren(children);
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // -> index 0
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // -> index 1
    expect(state.selectedIndex).toBe(1);

    state = reducer(state, { type: "NAVIGATE_DOWN" }); // -> index 2
    expect(state.selectedIndex).toBe(2);
  });

  it("NAVIGATE_DOWN clamps at end of list", () => {
    let state = stateWithChildren(children);
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 0
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 1
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 2
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // still 2

    expect(state.selectedIndex).toBe(2);
  });

  it("NAVIGATE_UP from first child returns to breadcrumb", () => {
    let state = stateWithChildren(children);
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // -> index 0
    state = reducer(state, { type: "NAVIGATE_UP" }); // -> breadcrumb

    expect(state.breadcrumbSelected).toBe(true);
    expect(state.currentFact).toBeNull();
    expect(state.rightPanelMode).toBe("summary");
  });

  it("NAVIGATE_UP at breadcrumb is no-op", () => {
    const state = stateWithChildren(children);
    const next = reducer(state, { type: "NAVIGATE_UP" });
    expect(next).toEqual(state);
  });

  it("selecting a fact shows it in right panel", () => {
    let state = stateWithChildren(children);
    // Navigate to the fact (index 2)
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 0: projects (world)
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 1: people (world)
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 2: note.md (fact)

    expect(state.currentFact).toBe("worlds/note.md");
    expect(state.rightPanelMode).toBe("fact");
  });

  it("selecting a world shows summary", () => {
    let state = stateWithChildren(children);
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 0: projects (world)

    expect(state.currentFact).toBeNull();
    expect(state.rightPanelMode).toBe("summary");
    expect(state.statsPath).toBe("worlds/projects");
  });

  it("OPEN_ITEM on a world navigates into it", () => {
    let state = stateWithChildren(children);
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 0: projects
    state = reducer(state, { type: "OPEN_ITEM" });

    expect(state.currentPath).toBe("worlds/projects");
    expect(state.selectedIndex).toBe(0);
    expect(state.breadcrumbSelected).toBe(true);
  });

  it("OPEN_ITEM on a fact shows it in right panel", () => {
    let state = stateWithChildren(children);
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 0
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 1
    state = reducer(state, { type: "NAVIGATE_DOWN" }); // 2: note.md
    state = reducer(state, { type: "OPEN_ITEM" });

    expect(state.currentFact).toBe("worlds/note.md");
    expect(state.rightPanelMode).toBe("fact");
  });

  it("GO_UP navigates to parent directory", () => {
    let state: AppState = {
      ...initialState,
      currentPath: "worlds/projects/webapp",
    };

    state = reducer(state, { type: "GO_UP" });
    expect(state.currentPath).toBe("worlds/projects");

    state = reducer(state, { type: "GO_UP" });
    expect(state.currentPath).toBe("worlds");
  });

  it("GO_UP at worlds root is no-op", () => {
    const state = reducer(initialState, { type: "GO_UP" });
    expect(state.currentPath).toBe("worlds");
  });

  it("GO_UP clears search state", () => {
    let state: AppState = {
      ...initialState,
      currentPath: "worlds/projects",
      searchActive: true,
      searchResults: [{ file: "x.md", title: "x", body: "x", score: 50 }],
    };

    state = reducer(state, { type: "GO_UP" });
    expect(state.searchActive).toBe(false);
    expect(state.searchResults).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

describe("TUI search", () => {
  const children: ChildItem[] = [
    { name: "projects", type: "world" },
    { name: "note.md", type: "fact" },
  ];

  it("SET_SEARCH_RESULTS activates search mode", () => {
    const state = stateWithChildren(children);
    const results = [
      { file: "worlds/a.md", title: "Result A", body: "Body A", score: 80 },
      { file: "worlds/b.md", title: "Result B", body: "Body B", score: 60 },
    ];

    const next = reducer(state, { type: "SET_SEARCH_RESULTS", results });
    expect(next.searchActive).toBe(true);
    expect(next.searchResults).toHaveLength(2);
    expect(next.selectedIndex).toBe(0);
    expect(next.breadcrumbSelected).toBe(false);
    expect(next.currentFact).toBe("worlds/a.md");
    expect(next.rightPanelMode).toBe("fact");
  });

  it("navigation works in search results", () => {
    let state = stateWithChildren(children);
    const results = [
      { file: "worlds/a.md", title: "A", body: "A", score: 80 },
      { file: "worlds/b.md", title: "B", body: "B", score: 60 },
    ];
    state = reducer(state, { type: "SET_SEARCH_RESULTS", results });

    state = reducer(state, { type: "NAVIGATE_DOWN" });
    expect(state.selectedIndex).toBe(1);
    expect(state.currentFact).toBe("worlds/b.md");
  });

  it("CLEAR_SEARCH restores previous navigation state", () => {
    let state = stateWithChildren(children);
    // Navigate to index 1 (note.md)
    state = reducer(state, { type: "NAVIGATE_DOWN" });
    state = reducer(state, { type: "NAVIGATE_DOWN" });
    expect(state.currentFact).toBe("worlds/note.md");

    // Search
    state = reducer(state, {
      type: "SET_SEARCH_RESULTS",
      results: [{ file: "worlds/x.md", title: "X", body: "X", score: 50 }],
    });
    expect(state.searchActive).toBe(true);

    // Clear search — should restore pre-search state
    state = reducer(state, { type: "CLEAR_SEARCH" });
    expect(state.searchActive).toBe(false);
    expect(state.searchResults).toEqual([]);
    expect(state.currentFact).toBe("worlds/note.md");
    expect(state.selectedIndex).toBe(1);
  });

  it("SET_SEARCH_RESULTS with empty results shows summary", () => {
    const state = stateWithChildren(children);
    const next = reducer(state, { type: "SET_SEARCH_RESULTS", results: [] });

    expect(next.searchActive).toBe(true);
    expect(next.currentFact).toBeNull();
    expect(next.rightPanelMode).toBe("summary");
  });

  it("preserves search type", () => {
    const state = stateWithChildren(children);
    const next = reducer(state, {
      type: "SET_SEARCH_RESULTS",
      results: [],
      searchType: "domain",
    });
    expect(next.searchType).toBe("domain");
  });
});

// ---------------------------------------------------------------------------
// History mode
// ---------------------------------------------------------------------------

describe("TUI history mode", () => {
  it("TOGGLE_HISTORY enables history mode", () => {
    const state = reducer(initialState, {
      type: "TOGGLE_HISTORY",
      target: "worlds/test/fact.md",
    });

    expect(state.historyMode).toBe(true);
    expect(state.historyTarget).toBe("worlds/test/fact.md");
    expect(state.rightPanelMode).toBe("history");
  });

  it("TOGGLE_HISTORY again disables history mode", () => {
    let state = reducer(initialState, {
      type: "TOGGLE_HISTORY",
      target: "worlds/test/fact.md",
    });
    state = reducer(state, { type: "TOGGLE_HISTORY", target: "" });

    expect(state.historyMode).toBe(false);
    expect(state.historyTarget).toBe("");
  });

  it("SET_HISTORY_ENTRIES populates history", () => {
    const entries: LogEntry[] = [
      { commit: "abc123", date: "2024-01-01", message: "learn: test" },
      { commit: "def456", date: "2024-01-02", message: "update: test" },
    ];

    let state = reducer(initialState, {
      type: "TOGGLE_HISTORY",
      target: "worlds/test/fact.md",
    });
    state = reducer(state, { type: "SET_HISTORY_ENTRIES", entries });

    expect(state.historyEntries).toHaveLength(2);
    expect(state.historySelectedIndex).toBe(0);
  });

  it("navigation in history mode moves through entries", () => {
    const entries: LogEntry[] = [
      { commit: "a", date: "2024-01-01", message: "1" },
      { commit: "b", date: "2024-01-02", message: "2" },
      { commit: "c", date: "2024-01-03", message: "3" },
    ];

    let state: AppState = {
      ...initialState,
      historyMode: true,
      historyEntries: entries,
      historySelectedIndex: 0,
    };

    state = reducer(state, { type: "NAVIGATE_DOWN" });
    expect(state.historySelectedIndex).toBe(1);

    state = reducer(state, { type: "NAVIGATE_DOWN" });
    expect(state.historySelectedIndex).toBe(2);

    state = reducer(state, { type: "NAVIGATE_DOWN" });
    expect(state.historySelectedIndex).toBe(2); // clamped

    state = reducer(state, { type: "NAVIGATE_UP" });
    expect(state.historySelectedIndex).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// Focus management
// ---------------------------------------------------------------------------

describe("TUI focus", () => {
  it("SET_FOCUS changes focus zone", () => {
    let state = reducer(initialState, { type: "SET_FOCUS", zone: "right" });
    expect(state.focusZone).toBe("right");
    expect(state.rightSelectedIndex).toBe(0);

    state = reducer(state, { type: "SET_FOCUS", zone: "left" });
    expect(state.focusZone).toBe("left");
  });

  it("right panel navigation works", () => {
    let state: AppState = {
      ...initialState,
      focusZone: "right",
      rightItemCount: 5,
      rightSelectedIndex: 0,
    };

    state = reducer(state, { type: "RIGHT_NAVIGATE_DOWN" });
    expect(state.rightSelectedIndex).toBe(1);

    state = reducer(state, { type: "RIGHT_NAVIGATE_DOWN" });
    expect(state.rightSelectedIndex).toBe(2);

    state = reducer(state, { type: "RIGHT_NAVIGATE_UP" });
    expect(state.rightSelectedIndex).toBe(1);
  });

  it("right navigation clamps at boundaries", () => {
    let state: AppState = {
      ...initialState,
      rightItemCount: 3,
      rightSelectedIndex: 2,
    };

    state = reducer(state, { type: "RIGHT_NAVIGATE_DOWN" });
    expect(state.rightSelectedIndex).toBe(2); // clamped at max

    state = { ...state, rightSelectedIndex: 0 };
    state = reducer(state, { type: "RIGHT_NAVIGATE_UP" });
    expect(state.rightSelectedIndex).toBe(0); // clamped at 0
  });

  it("SET_RIGHT_ITEM_COUNT adjusts selected index", () => {
    let state: AppState = {
      ...initialState,
      rightItemCount: 10,
      rightSelectedIndex: 8,
    };

    // Shrink item count — selected index should clamp
    state = reducer(state, { type: "SET_RIGHT_ITEM_COUNT", count: 5 });
    expect(state.rightItemCount).toBe(5);
    expect(state.rightSelectedIndex).toBe(4); // clamped to count-1
  });
});

// ---------------------------------------------------------------------------
// Loading state
// ---------------------------------------------------------------------------

describe("TUI loading", () => {
  it("SET_LOADING toggles loading state", () => {
    const loading = reducer(initialState, { type: "SET_LOADING", loading: true });
    expect(loading.loading).toBe(true);

    const done = reducer(loading, { type: "SET_LOADING", loading: false });
    expect(done.loading).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Complex state transitions
// ---------------------------------------------------------------------------

describe("TUI complex workflows", () => {
  it("navigate → search → clear → navigate restores position", () => {
    const children: ChildItem[] = [
      { name: "a", type: "world" },
      { name: "b", type: "world" },
      { name: "c.md", type: "fact", summary: "C" },
    ];

    const state = applyActions(initialState, [
      { type: "SET_CHILDREN", children },
      { type: "NAVIGATE_DOWN" }, // index 0
      { type: "NAVIGATE_DOWN" }, // index 1
      { type: "NAVIGATE_DOWN" }, // index 2, fact c.md
      { type: "SET_SEARCH_RESULTS", results: [
        { file: "worlds/x.md", title: "X", body: "X", score: 90 },
      ]},
      { type: "NAVIGATE_DOWN" }, // Still at index 0 in search (only 1 result)
      { type: "CLEAR_SEARCH" },
    ]);

    // Should restore to index 2, fact c.md
    expect(state.searchActive).toBe(false);
    expect(state.selectedIndex).toBe(2);
    expect(state.currentFact).toBe("worlds/c.md");
  });

  it("deep navigation with multiple level changes", () => {
    const state = applyActions(initialState, [
      // Set children at root
      { type: "SET_CHILDREN", children: [
        { name: "projects", type: "world" as const },
      ]},
      { type: "NAVIGATE_DOWN" }, // select projects
      { type: "OPEN_ITEM" }, // enter projects
      // Now at worlds/projects
      { type: "SET_CHILDREN", children: [
        { name: "webapp", type: "world" as const },
      ]},
      { type: "NAVIGATE_DOWN" }, // select webapp
      { type: "OPEN_ITEM" }, // enter webapp
      // Now at worlds/projects/webapp
    ]);

    expect(state.currentPath).toBe("worlds/projects/webapp");

    // Go back up
    const backUp = applyActions(state, [
      { type: "GO_UP" },
      { type: "GO_UP" },
    ]);
    expect(backUp.currentPath).toBe("worlds");
  });
});
