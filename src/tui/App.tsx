import React, { useReducer, useEffect, useCallback, useState } from "react";
import { render, Box, useInput, useApp, useStdout } from "ink";
import type { GitRepo } from "../git.js";
import type { SearchIndex } from "../search-index.js";
import { parseFact } from "../facts.js";
import type { Frontmatter } from "../facts.js";
import type { LogEntry } from "../git.js";
import type { StatsResult } from "../search-index.js";
import { defaultTheme } from "./theme.js";
import { reducer, initialState } from "./state.js";
import { TopBar } from "./TopBar.js";
import { LeftPanel } from "./LeftPanel.js";
import { RightPanel } from "./RightPanel.js";
import { CommandBar } from "./CommandBar.js";
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

  const branch = repo.branchName;

  // Load children when path changes
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
  }, [state.currentPath, state.searchActive]);

  // Load stats when statsPath changes and in summary mode
  useEffect(() => {
    if (state.rightPanelMode !== "summary") return;
    try {
      const s = searchIndex.stats(state.statsPath);
      setStats(s);
    } catch {
      setStats(null);
    }
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
      dispatch({ type: "GO_UP" });
    }
    else if (key.backspace || key.delete) {
      dispatch({ type: "GO_UP" });
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
        results: results.map((r) => ({ file: r.path, title: r.title, body: r.body })),
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
        />
        <RightPanel
          mode={state.rightPanelMode}
          theme={theme}
          stats={stats}
          factTitle={factTitle}
          factBody={factBody}
          factFrontmatter={factFrontmatter}
          history={history}
          historyFile={state.currentFact ?? state.statsPath}
        />
      </Box>
      <CommandBar
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
