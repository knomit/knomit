import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";
import type { Frontmatter } from "../facts.js";
import type { StatsResult } from "../search-index.js";
import type { LogEntry } from "../git.js";

interface RightPanelProps {
  mode: "summary" | "fact" | "history";
  theme: Theme;
  stats?: StatsResult | null;
  factTitle?: string;
  factBody?: string;
  factFrontmatter?: Frontmatter;
  history?: LogEntry[];
  historyFile?: string;
}

export function RightPanel({
  mode, theme, stats, factTitle, factBody, factFrontmatter, history, historyFile,
}: RightPanelProps) {
  return (
    <Box flexDirection="column" width="60%" borderStyle="single" overflow="hidden">
      <Box paddingX={1} flexDirection="column">
        {mode === "summary" && <SummaryView stats={stats} theme={theme} />}
        {mode === "fact" && (
          <FactView title={factTitle ?? ""} body={factBody ?? ""} frontmatter={factFrontmatter} theme={theme} />
        )}
        {mode === "history" && (
          <HistoryView entries={history ?? []} file={historyFile ?? ""} theme={theme} />
        )}
      </Box>
    </Box>
  );
}

function SummaryView({ stats, theme }: { stats?: StatsResult | null; theme: Theme }) {
  if (!stats) return <Text dimColor>Loading...</Text>;
  if (stats.totalFacts === 0) return <Text dimColor>No facts in this subtree.</Text>;

  const domains = Object.entries(stats.domainCounts)
    .sort((a, b) => b[1] - a[1])
    .slice(0, 10);

  return (
    <Box flexDirection="column">
      <Text bold color={theme.primary}>Summary</Text>
      <Text> </Text>
      <Text>Total facts: <Text bold>{stats.totalFacts}</Text></Text>
      <Text>Avg confidence: <Text bold>{stats.avgConfidence.toFixed(2)}</Text></Text>
      {domains.length > 0 && (
        <>
          <Text> </Text>
          <Text bold>Domains:</Text>
          {domains.map(([domain, count]) => (
            <Text key={domain}>  {domain}: {count}</Text>
          ))}
        </>
      )}
    </Box>
  );
}

function FactView({ title, body, frontmatter, theme }: {
  title: string; body: string; frontmatter?: Frontmatter; theme: Theme;
}) {
  return (
    <Box flexDirection="column">
      <Text bold color={theme.success}># {title}</Text>
      {frontmatter && (
        <Text dimColor>
          confidence: {frontmatter.confidence}  sources: {frontmatter.sources}
          {frontmatter.domain.length > 0 ? `  domain: ${frontmatter.domain.join(", ")}` : ""}
          {frontmatter.entities.length > 0 ? `  entities: ${frontmatter.entities.join(", ")}` : ""}
        </Text>
      )}
      <Text> </Text>
      <Text>{body}</Text>
      {frontmatter && frontmatter.refs.length > 0 && (
        <Box flexDirection="column" marginTop={1}>
          <Text dimColor bold>refs:</Text>
          {frontmatter.refs.map((r) => (
            <Text key={r} dimColor>  → {r}</Text>
          ))}
        </Box>
      )}
    </Box>
  );
}

function HistoryView({ entries, file, theme }: {
  entries: LogEntry[]; file: string; theme: Theme;
}) {
  return (
    <Box flexDirection="column">
      <Text bold color={theme.primary}>History</Text>
      <Text dimColor>{file}</Text>
      <Text> </Text>
      {entries.length === 0 ? (
        <Text dimColor>No history found.</Text>
      ) : (
        entries.map((e) => (
          <Text key={e.commit}>
            <Text color="yellow">{e.commit.slice(0, 7)}</Text>
            {"  "}
            <Text dimColor>{new Date(e.date).toLocaleDateString()}</Text>
            {"  "}
            <Text>{e.message}</Text>
          </Text>
        ))
      )}
      <Text> </Text>
      <Text dimColor>Press h to go back</Text>
    </Box>
  );
}
