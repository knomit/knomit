export interface ChildItem {
  name: string;
  type: "world" | "fact";
  summary?: string;
}

export interface SearchResultItem {
  file: string;
  title: string;
  body: string;
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
  focusZone: "left" | "command";
  loading: boolean;
}

export const initialState: AppState = {
  currentPath: "worlds",
  selectedIndex: 0,
  breadcrumbSelected: true,
  currentFact: null,
  statsPath: "worlds",
  rightPanelMode: "summary",
  searchActive: false,
  searchResults: [],
  children: [],
  focusZone: "left",
  loading: false,
};

export type Action =
  | { type: "SET_CHILDREN"; children: ChildItem[] }
  | { type: "NAVIGATE_UP" }
  | { type: "NAVIGATE_DOWN" }
  | { type: "OPEN_ITEM" }
  | { type: "GO_UP" }
  | { type: "SET_FOCUS"; zone: "left" | "command" }
  | { type: "TOGGLE_HISTORY" }
  | { type: "SET_SEARCH_RESULTS"; results: SearchResultItem[] }
  | { type: "CLEAR_SEARCH" }
  | { type: "SELECT_SEARCH_RESULT" }
  | { type: "NAVIGATE_TO_PATH"; path: string }
  | { type: "SET_LOADING"; loading: boolean };

function autoSelectItem(
  state: AppState,
  index: number
): Pick<AppState, "currentFact" | "rightPanelMode" | "statsPath" | "breadcrumbSelected"> {
  const sticky = state.rightPanelMode === "history";
  if (state.searchActive) {
    const item = state.searchResults[index];
    return item
      ? { currentFact: item.file, rightPanelMode: sticky ? "history" : "fact", statsPath: state.currentPath, breadcrumbSelected: false }
      : { currentFact: null, rightPanelMode: sticky ? "history" : "summary", statsPath: state.currentPath, breadcrumbSelected: false };
  }
  const child = state.children[index];
  if (child?.type === "fact") {
    return {
      currentFact: `${state.currentPath}/${child.name}`,
      rightPanelMode: sticky ? "history" : "fact",
      statsPath: state.currentPath,
      breadcrumbSelected: false,
    };
  }
  if (child?.type === "world") {
    return {
      currentFact: null,
      rightPanelMode: sticky ? "history" : "summary",
      statsPath: `${state.currentPath}/${child.name}`,
      breadcrumbSelected: false,
    };
  }
  return { currentFact: null, rightPanelMode: sticky ? "history" : "summary", statsPath: state.currentPath, breadcrumbSelected: false };
}

export function reducer(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "SET_CHILDREN": {
      const mode = state.rightPanelMode === "history" ? "history" : "summary";
      return {
        ...state,
        children: action.children,
        selectedIndex: 0,
        breadcrumbSelected: true,
        statsPath: state.currentPath,
        currentFact: null,
        rightPanelMode: mode,
      };
    }

    case "NAVIGATE_DOWN": {
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
      if (state.breadcrumbSelected) return state;
      if (state.selectedIndex === 0) {
        const sticky = state.rightPanelMode === "history";
        return {
          ...state,
          breadcrumbSelected: true,
          currentFact: null,
          rightPanelMode: sticky ? "history" : "summary",
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
          rightPanelMode: state.rightPanelMode === "history" ? "history" : "summary",
        };
      }
      return {
        ...state,
        currentFact: `${state.currentPath}/${child.name}`,
        rightPanelMode: "fact",
      };
    }

    case "GO_UP": {
      if (state.currentPath === "worlds") return state;
      const lastSlash = state.currentPath.lastIndexOf("/");
      const parentPath = lastSlash > 0 ? state.currentPath.slice(0, lastSlash) : "worlds";
      return {
        ...state,
        currentPath: parentPath,
        selectedIndex: 0,
        breadcrumbSelected: true,
        currentFact: null,
        statsPath: parentPath,
        rightPanelMode: state.rightPanelMode === "history" ? "history" : "summary",
        searchActive: false,
        searchResults: [],
      };
    }

    case "NAVIGATE_TO_PATH": {
      return {
        ...state,
        currentPath: action.path,
        selectedIndex: 0,
        breadcrumbSelected: true,
        currentFact: null,
        statsPath: action.path,
        rightPanelMode: state.rightPanelMode === "history" ? "history" : "summary",
        searchActive: false,
        searchResults: [],
      };
    }

    case "SET_FOCUS":
      return { ...state, focusZone: action.zone };

    case "TOGGLE_HISTORY": {
      if (state.rightPanelMode === "history") {
        return {
          ...state,
          rightPanelMode: state.currentFact ? "fact" : "summary",
        };
      }
      return { ...state, rightPanelMode: "history" };
    }

    case "SET_SEARCH_RESULTS": {
      const firstResult = action.results[0];
      return {
        ...state,
        searchActive: true,
        searchResults: action.results,
        selectedIndex: 0,
        breadcrumbSelected: false,
        focusZone: "left",
        currentFact: firstResult?.file ?? null,
        rightPanelMode: firstResult ? "fact" : "summary",
      };
    }

    case "CLEAR_SEARCH":
      return {
        ...state,
        searchActive: false,
        searchResults: [],
        selectedIndex: 0,
      };

    case "SELECT_SEARCH_RESULT": {
      const item = state.searchResults[state.selectedIndex];
      if (!item) return state;
      return {
        ...state,
        currentFact: item.file,
        rightPanelMode: "fact",
      };
    }

    case "SET_LOADING":
      return { ...state, loading: action.loading };
  }
}
