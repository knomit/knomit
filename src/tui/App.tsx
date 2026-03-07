import React, { useReducer, useEffect, useCallback, useState, useRef } from "react";
import { render, Box, Text, useInput, useApp, useStdout } from "ink";
import type { GitRepo } from "../git.js";
import type { SearchIndex } from "../search-index.js";
import { parseFact } from "../facts.js";
import type { Frontmatter } from "../facts.js";
import type { LogEntry } from "../git.js";
import type { StatsResult } from "../search-index.js";
import type { SummaryChild, RightSelectableItem } from "./RightPanel.js";
import { defaultTheme } from "./theme.js";
import { reducer, initialState, type ChildItem } from "./state.js";
import { TopBar } from "./TopBar.js";
import { LeftPanel } from "./LeftPanel.js";
import { RightPanel } from "./RightPanel.js";
import { StatusBar } from "./StatusBar.js";
import { exploreHandler } from "../tools/explore.js";

const theme = defaultTheme;

function App({ repo, searchIndex }: { repo: GitRepo; searchIndex: SearchIndex }) {
  const [state, dispatch] = useReducer(reducer, initialState);
  const { exit } = useApp();
  const { stdout } = useStdout();
  const [inputKey, setInputKey] = useState(0);

  // Track terminal size
  const [termSize, setTermSize] = useState({
    columns: stdout?.columns ?? 80,
    rows: stdout?.rows ?? 24,
  });
  useEffect(() => {
    if (!stdout) return;
    const onResize = () => setTermSize({ columns: stdout.columns, rows: stdout.rows });
    stdout.on("resize", onResize);
    return () => { stdout.off("resize", onResize); };
  }, [stdout]);

  // Right panel data
  const [factTitle, setFactTitle] = useState("");
  const [factBody, setFactBody] = useState("");
  const [factFrontmatter, setFactFrontmatter] = useState<Frontmatter | undefined>();
  const [history, setHistory] = useState<LogEntry[]>([]);
  const [stats, setStats] = useState<StatsResult | null>(null);
  const [summaryChildren, setSummaryChildren] = useState<SummaryChild[]>([]);

  const rightItemsRef = useRef<RightSelectableItem[]>([]);

  const handleRightItemsChanged = useCallback((items: RightSelectableItem[]) => {
    rightItemsRef.current = items;
    dispatch({ type: "SET_RIGHT_ITEM_COUNT", count: items.length });
  }, []);

  const branch = repo.branchName;

  // Load children when path changes (not when toggling search — children are preserved)
  useEffect(() => {
    if (state.searchActive) return;
    (async () => {
      try {
        const result = await exploreHandler(repo, { path: state.currentPath }, { skipSync: true });
        dispatch({ type: "SET_CHILDREN", children: result.children });
      } catch {
        dispatch({ type: "SET_CHILDREN", children: [] });
      }
    })();
  }, [state.currentPath]);

  // Load stats and fact summaries when statsPath changes and in summary mode
  useEffect(() => {
    if (state.rightPanelMode !== "summary") return;
    try {
      const s = searchIndex.stats(state.statsPath);
      setStats(s);
    } catch {
      setStats(null);
    }
    (async () => {
      try {
        const result = await exploreHandler(repo, { path: state.statsPath }, { skipSync: true });
        setSummaryChildren(
          result.children.map((c: ChildItem) => ({ name: c.name, type: c.type, summary: c.summary }))
        );
      } catch {
        setSummaryChildren([]);
      }
    })();
  }, [state.statsPath, state.rightPanelMode]);

  // Load fact content when currentFact changes
  useEffect(() => {
    if (!state.currentFact || state.rightPanelMode === "history") return;
    (async () => {
      try {
        const raw = await repo.readFile(state.currentFact!);
        const parsed = parseFact(raw);
        setFactTitle(parsed.title);
        setFactBody(parsed.body);
        setFactFrontmatter(parsed.frontmatter);
      } catch {
        setFactTitle("Error loading fact");
        setFactBody("");
        setFactFrontmatter(undefined);
      }
    })();
  }, [state.currentFact, state.rightPanelMode]);

  // Load history when toggled
  const historyTarget = state.currentFact ?? state.statsPath;
  useEffect(() => {
    if (state.rightPanelMode !== "history") return;
    (async () => {
      try {
        const entries = await repo.log(historyTarget);
        setHistory(entries);
      } catch {
        setHistory([]);
      }
    })();
  }, [state.rightPanelMode, historyTarget]);

  // Keyboard handling
  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }

    if (state.focusZone === "command") {
      if (key.escape) {
        dispatch({ type: "SET_FOCUS", zone: "left" });
      }
      return;
    }

    if (state.focusZone === "right") {
      if (key.leftArrow || key.escape) {
        dispatch({ type: "SET_FOCUS", zone: "left" });
      } else if (key.upArrow) {
        dispatch({ type: "RIGHT_NAVIGATE_UP" });
      } else if (key.downArrow) {
        dispatch({ type: "RIGHT_NAVIGATE_DOWN" });
      } else if (key.return) {
        const item = rightItemsRef.current[state.rightSelectedIndex];
        if (item) {
          if (item.type === "domain" || item.type === "entity") {
            handleDomainSearch(item.label);
          } else if (item.type === "fact" && item.path) {
            dispatch({ type: "SET_FOCUS", zone: "left" });
            const factPath = `${state.statsPath}/${item.path}`;
            dispatch({
              type: "SET_SEARCH_RESULTS",
              results: [{ file: factPath, title: item.label, body: "", score: 0 }],
              searchType: "domain",
            });
          }
        }
      } else if (input === "q") {
        exit();
      } else if (input === "h") {
        dispatch({ type: "TOGGLE_HISTORY" });
      }
      return;
    }

    // Left panel focused
    if (input === "q") {
      exit();
      return;
    }
    if (key.upArrow) dispatch({ type: "NAVIGATE_UP" });
    else if (key.downArrow) dispatch({ type: "NAVIGATE_DOWN" });
    else if (key.return) {
      if (state.breadcrumbSelected) {
        dispatch({ type: "GO_UP" });
      } else {
        dispatch({ type: "OPEN_ITEM" });
      }
    }
    else if (key.leftArrow) {
      if (state.searchActive) {
        dispatch({ type: "CLEAR_SEARCH" });
      } else {
        dispatch({ type: "GO_UP" });
      }
    }
    else if (key.rightArrow) {
      if ((state.rightPanelMode === "summary" || state.rightPanelMode === "fact") && state.rightItemCount > 0) {
        dispatch({ type: "SET_FOCUS", zone: "right" });
      }
    }
    else if (key.backspace || key.delete) {
      if (state.searchActive) {
        dispatch({ type: "CLEAR_SEARCH" });
      } else {
        dispatch({ type: "GO_UP" });
      }
    }
    else if (input === "/") {
      dispatch({ type: "SET_FOCUS", zone: "command" });
    }
    else if (key.tab) {
      dispatch({ type: "SET_FOCUS", zone: "command" });
    }
    else if (input === "h") {
      dispatch({ type: "TOGGLE_HISTORY" });
    }
    else if (key.escape && state.searchActive) {
      dispatch({ type: "CLEAR_SEARCH" });
    }
  });

  const handleDomainSearch = useCallback(async (domain: string) => {
    try {
      dispatch({ type: "SET_LOADING", loading: true });
      dispatch({ type: "SET_FOCUS", zone: "left" });
      const results = await searchIndex.search({ text: domain, min_confidence: 0 });
      dispatch({
        type: "SET_SEARCH_RESULTS",
        results: results.map((r) => ({
          file: r.path,
          title: r.title,
          body: r.body,
          score: r.score,
        })),
        searchType: "domain",
      });
    } catch {
      // ignore
    } finally {
      dispatch({ type: "SET_LOADING", loading: false });
    }
  }, [searchIndex]);

  const handleCommandSubmit = useCallback(async (text: string) => {
    const trimmed = text.trim();
    if (!trimmed) return;

    setInputKey((k: number) => k + 1);
    dispatch({ type: "SET_FOCUS", zone: "left" });

    try {
      dispatch({ type: "SET_LOADING", loading: true });
      const results = await searchIndex.search({ text: trimmed, min_confidence: 0 });
      dispatch({
        type: "SET_SEARCH_RESULTS",
        results: results.map((r) => ({
          file: r.path,
          title: r.title,
          body: r.body,
          score: r.score,
        })),
      });
    } catch {
      // ignore search errors
    } finally {
      dispatch({ type: "SET_LOADING", loading: false });
    }
  }, [repo, searchIndex]);

  return (
    <Box flexDirection="column" width={termSize.columns} height={termSize.rows}>
      <TopBar branch={branch} theme={theme} />
      <Box flexDirection="row" flexGrow={1} overflow="hidden">
        <LeftPanel
          currentPath={state.currentPath}
          children={state.children}
          searchActive={state.searchActive}
          searchResults={state.searchResults}
          selectedIndex={state.selectedIndex}
          breadcrumbSelected={state.breadcrumbSelected}
          focused={state.focusZone === "left"}
          theme={theme}
          searchType={state.searchType}
          statusText={
            state.searchActive && !state.breadcrumbSelected
              ? state.searchResults[state.selectedIndex]?.file
              : undefined
          }
        />
        <RightPanel
          mode={state.rightPanelMode}
          theme={theme}
          stats={stats}
          summaryChildren={state.rightPanelMode === "summary" ? summaryChildren : undefined}
          factTitle={factTitle}
          factBody={factBody}
          factFrontmatter={factFrontmatter}
          history={history}
          historyFile={state.currentFact ?? state.statsPath}
          focused={state.focusZone === "right"}
          selectedIndex={state.rightSelectedIndex}
          onItemsChanged={handleRightItemsChanged}
        />
      </Box>
      <StatusBar
        focused={state.focusZone === "command"}
        theme={theme}
        onSubmit={handleCommandSubmit}
        inputKey={inputKey}
      />
    </Box>
  );
}

export function startApp(repo: GitRepo, searchIndex: SearchIndex) {
  // Enter alternate screen buffer (like vim/htop) — terminal restores on exit
  process.stdout.write("\x1b[?1049h");

  const instance = render(<App repo={repo} searchIndex={searchIndex} />, {
    exitOnCtrlC: false,
  });

  const leave = () => {
    process.stdout.write("\x1b[?1049l");
    process.exit(0);
  };

  process.on("SIGINT", leave);
  process.on("SIGTERM", leave);

  instance.waitUntilExit().then(leave);
}
