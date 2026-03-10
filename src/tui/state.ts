import type { LogEntry } from "../git.js";
import { ONTOLOGY_DIR } from "../constants.js";

export interface ChildItem {
  name: string;
  type: "world" | "fact";
  summary?: string;
}

export interface SearchResultItem {
  file: string;
  title: string;
  body: string;
  score: number;
}

export interface SavedNavState {
  selectedIndex: number;
  breadcrumbSelected: boolean;
  currentFact: string | null;
  statsPath: string;
  rightPanelMode: "summary" | "fact" | "history";
}

export interface NavEntry {
  currentPath: string;
  currentFact: string | null;
  selectedIndex: number;
  breadcrumbSelected: boolean;
  statsPath: string;
  rightPanelMode: "summary" | "fact" | "history";
  historyMode: boolean;
  historyTarget: string;
  historySelectedIndex: number;
  focusZone: "left" | "right";
  rightSelectedIndex: number;
  searchActive: boolean;
  searchResults: SearchResultItem[];
  searchType: "text" | "domain";
  savedNavState: SavedNavState | null;
}

export interface AppState {
  currentPath: string;
  selectedIndex: number;
  breadcrumbSelected: boolean;
  currentFact: string | null;
  statsPath: string;
  rightPanelMode: "summary" | "fact" | "history";
  searchActive: boolean;
  searchResults: SearchResultItem[];
  children: ChildItem[];
  focusZone: "left" | "right" | "command" | "cmdline";
  rightSelectedIndex: number;
  rightItemCount: number;
  searchType: "text" | "domain";
  loading: boolean;
  savedNavState: SavedNavState | null;
  historyMode: boolean;
  historyEntries: LogEntry[];
  historyTarget: string;
  historySelectedIndex: number;
  navStack: NavEntry[];
}

export const initialState: AppState = {
  currentPath: ONTOLOGY_DIR,
  selectedIndex: 0,
  breadcrumbSelected: true,
  currentFact: null,
  statsPath: ONTOLOGY_DIR,
  rightPanelMode: "summary",
  searchActive: false,
  searchResults: [],
  children: [],
  focusZone: "left",
  rightSelectedIndex: 0,
  rightItemCount: 0,
  searchType: "text",
  loading: false,
  savedNavState: null,
  historyMode: false,
  historyEntries: [],
  historyTarget: "",
  historySelectedIndex: 0,
  navStack: [],
};

export type Action =
  | { type: "SET_CHILDREN"; children: ChildItem[] }
  | { type: "NAVIGATE_UP" }
  | { type: "NAVIGATE_DOWN" }
  | { type: "OPEN_ITEM" }
  | { type: "GO_UP" }
  | { type: "SET_FOCUS"; zone: "left" | "right" | "command" | "cmdline" }
  | { type: "TOGGLE_HISTORY"; target: string }
  | { type: "SET_HISTORY_ENTRIES"; entries: LogEntry[] }
  | { type: "SET_SEARCH_RESULTS"; results: SearchResultItem[]; searchType?: "text" | "domain" }
  | { type: "CLEAR_SEARCH" }
  | { type: "SET_LOADING"; loading: boolean }
  | { type: "RIGHT_NAVIGATE_UP" }
  | { type: "RIGHT_NAVIGATE_DOWN" }
  | { type: "SET_RIGHT_ITEM_COUNT"; count: number }
  | { type: "FOLLOW_REF"; path: string; commit: string }
  | { type: "NAV_BACK" }
  | { type: "SET_HISTORY_SELECTED_INDEX"; index: number };

function autoSelectItem(
  state: AppState,
  index: number
): Pick<AppState, "currentFact" | "rightPanelMode" | "statsPath" | "breadcrumbSelected"> {
  if (state.searchActive) {
    const item = state.searchResults[index];
    return item
      ? { currentFact: item.file, rightPanelMode: "fact", statsPath: state.currentPath, breadcrumbSelected: false }
      : { currentFact: null, rightPanelMode: "summary", statsPath: state.currentPath, breadcrumbSelected: false };
  }
  const child = state.children[index];
  if (child?.type === "fact") {
    return {
      currentFact: `${state.currentPath}/${child.name}`,
      rightPanelMode: "fact",
      statsPath: state.currentPath,
      breadcrumbSelected: false,
    };
  }
  if (child?.type === "world") {
    return {
      currentFact: null,
      rightPanelMode: "summary",
      statsPath: `${state.currentPath}/${child.name}`,
      breadcrumbSelected: false,
    };
  }
  return { currentFact: null, rightPanelMode: "summary", statsPath: state.currentPath, breadcrumbSelected: false };
}

export function reducer(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "SET_CHILDREN": {
      return {
        ...state,
        children: action.children,
        selectedIndex: 0,
        breadcrumbSelected: true,
        statsPath: state.currentPath,
        currentFact: null,
        rightPanelMode: "summary",
      };
    }

    case "NAVIGATE_DOWN": {
      if (state.historyMode) {
        const maxIdx = state.historyEntries.length - 1;
        if (maxIdx < 0) return state;
        return {
          ...state,
          historySelectedIndex: Math.min(state.historySelectedIndex + 1, maxIdx),
        };
      }
      if (state.breadcrumbSelected) {
        if ((state.searchActive ? state.searchResults.length : state.children.length) === 0) return state;
        return {
          ...state,
          breadcrumbSelected: false,
          selectedIndex: 0,
          ...autoSelectItem(state, 0),
        };
      }
      const maxIndex = (state.searchActive ? state.searchResults.length : state.children.length) - 1;
      const newIndex = Math.min(state.selectedIndex + 1, Math.max(0, maxIndex));
      return {
        ...state,
        selectedIndex: newIndex,
        ...autoSelectItem(state, newIndex),
      };
    }

    case "NAVIGATE_UP": {
      if (state.historyMode) {
        return {
          ...state,
          historySelectedIndex: Math.max(state.historySelectedIndex - 1, 0),
        };
      }
      if (state.breadcrumbSelected) return state;
      if (state.selectedIndex === 0) {
        return {
          ...state,
          breadcrumbSelected: true,
          currentFact: null,
          rightPanelMode: "summary",
          statsPath: state.currentPath,
        };
      }
      const newIndex = state.selectedIndex - 1;
      return {
        ...state,
        selectedIndex: newIndex,
        ...autoSelectItem(state, newIndex),
      };
    }

    case "OPEN_ITEM": {
      if (state.breadcrumbSelected) return state;
      if (state.searchActive) {
        const item = state.searchResults[state.selectedIndex];
        if (!item) return state;
        return {
          ...state,
          currentFact: item.file,
          rightPanelMode: "fact",
        };
      }
      const child = state.children[state.selectedIndex];
      if (!child) return state;
      if (child.type === "world") {
        return {
          ...state,
          currentPath: `${state.currentPath}/${child.name}`,
          selectedIndex: 0,
          breadcrumbSelected: true,
          currentFact: null,
          statsPath: `${state.currentPath}/${child.name}`,
          rightPanelMode: "summary",
        };
      }
      return {
        ...state,
        currentFact: `${state.currentPath}/${child.name}`,
        rightPanelMode: "fact",
      };
    }

    case "GO_UP": {
      if (state.currentPath === ONTOLOGY_DIR) return state;
      const lastSlash = state.currentPath.lastIndexOf("/");
      const parentPath = lastSlash > 0 ? state.currentPath.slice(0, lastSlash) : ONTOLOGY_DIR;
      return {
        ...state,
        currentPath: parentPath,
        selectedIndex: 0,
        breadcrumbSelected: true,
        currentFact: null,
        statsPath: parentPath,
        rightPanelMode: "summary",
        searchActive: false,
        searchResults: [],
      };
    }

    case "SET_FOCUS":
      return {
        ...state,
        focusZone: action.zone,
        ...(action.zone === "right" ? { rightSelectedIndex: 0 } : {}),
      };

    case "TOGGLE_HISTORY": {
      if (state.historyMode) {
        return {
          ...state,
          historyMode: false,
          historyEntries: [],
          historyTarget: "",
          historySelectedIndex: 0,
          rightPanelMode: state.currentFact ? "fact" : "summary",
        };
      }
      return {
        ...state,
        historyMode: true,
        historyTarget: action.target,
        historySelectedIndex: 0,
        rightPanelMode: "history",
      };
    }

    case "SET_HISTORY_ENTRIES": {
      return {
        ...state,
        historyEntries: action.entries,
        historySelectedIndex: 0,
      };
    }

    case "SET_SEARCH_RESULTS": {
      const firstResult = action.results[0];
      return {
        ...state,
        searchActive: true,
        searchResults: action.results,
        searchType: action.searchType ?? "text",
        selectedIndex: 0,
        breadcrumbSelected: false,
        focusZone: "left",
        currentFact: firstResult?.file ?? null,
        rightPanelMode: firstResult ? "fact" : "summary",
        savedNavState: state.savedNavState ?? {
          selectedIndex: state.selectedIndex,
          breadcrumbSelected: state.breadcrumbSelected,
          currentFact: state.currentFact,
          statsPath: state.statsPath,
          rightPanelMode: state.rightPanelMode,
        },
      };
    }

    case "CLEAR_SEARCH": {
      const saved = state.savedNavState;
      return {
        ...state,
        searchActive: false,
        searchResults: [],
        selectedIndex: saved?.selectedIndex ?? 0,
        breadcrumbSelected: saved?.breadcrumbSelected ?? true,
        currentFact: saved?.currentFact ?? null,
        statsPath: saved?.statsPath ?? state.currentPath,
        rightPanelMode: saved?.rightPanelMode ?? "summary",
        savedNavState: null,
      };
    }

    case "SET_LOADING":
      return { ...state, loading: action.loading };

    case "RIGHT_NAVIGATE_DOWN": {
      if (state.rightItemCount === 0) return state;
      return {
        ...state,
        rightSelectedIndex: Math.min(state.rightSelectedIndex + 1, state.rightItemCount - 1),
      };
    }

    case "RIGHT_NAVIGATE_UP": {
      if (state.rightItemCount === 0) return state;
      return {
        ...state,
        rightSelectedIndex: Math.max(state.rightSelectedIndex - 1, 0),
      };
    }

    case "SET_RIGHT_ITEM_COUNT":
      return {
        ...state,
        rightItemCount: action.count,
        rightSelectedIndex: Math.min(state.rightSelectedIndex, Math.max(0, action.count - 1)),
      };

    case "FOLLOW_REF": {
      const entry: NavEntry = {
        currentPath: state.currentPath,
        currentFact: state.currentFact,
        selectedIndex: state.selectedIndex,
        breadcrumbSelected: state.breadcrumbSelected,
        statsPath: state.statsPath,
        rightPanelMode: state.rightPanelMode,
        historyMode: state.historyMode,
        historyTarget: state.historyTarget,
        historySelectedIndex: state.historySelectedIndex,
        focusZone: state.focusZone as "left" | "right",
        rightSelectedIndex: state.rightSelectedIndex,
        searchActive: state.searchActive,
        searchResults: state.searchResults,
        searchType: state.searchType,
        savedNavState: state.savedNavState,
      };
      return {
        ...state,
        navStack: [...state.navStack, entry],
        historyMode: true,
        historyTarget: action.path,
        historySelectedIndex: 0,
        historyEntries: [],
        currentFact: action.path,
        rightPanelMode: "history",
        focusZone: "left",
        rightSelectedIndex: 0,
      };
    }

    case "SET_HISTORY_SELECTED_INDEX":
      return { ...state, historySelectedIndex: action.index };

    case "NAV_BACK": {
      if (state.navStack.length === 0) return state;
      const stack = [...state.navStack];
      const prev = stack.pop()!;
      return {
        ...state,
        navStack: stack,
        currentPath: prev.currentPath,
        currentFact: prev.currentFact,
        selectedIndex: prev.selectedIndex,
        breadcrumbSelected: prev.breadcrumbSelected,
        statsPath: prev.statsPath,
        rightPanelMode: prev.rightPanelMode,
        historyMode: prev.historyMode,
        historyTarget: prev.historyTarget,
        historySelectedIndex: prev.historySelectedIndex,
        focusZone: prev.focusZone,
        rightSelectedIndex: prev.rightSelectedIndex,
        searchActive: prev.searchActive,
        searchResults: prev.searchResults,
        searchType: prev.searchType,
        savedNavState: prev.savedNavState,
        historyEntries: [],
      };
    }
  }
}
