import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";
import { glyph, box } from "./theme.js";
import type { Frontmatter } from "../facts.js";
import type { StatsResult } from "../search-index.js";
import type { LogEntry } from "../git.js";

export interface FactSummaryItem {
  name: string;
  summary?: string;
}

interface RightPanelProps {
  mode: "summary" | "fact" | "history";
  theme: Theme;
  stats?: StatsResult | null;
  factSummaries?: FactSummaryItem[];
  factTitle?: string;
  factBody?: string;
  factFrontmatter?: Frontmatter;
  history?: LogEntry[];
  historyFile?: string;
}

export function RightPanel({
  mode, theme, stats, factSummaries, factTitle, factBody, factFrontmatter, history, historyFile,
}: RightPanelProps) {
  return (
    <Box flexDirection="column" width="60%" borderStyle="round" borderColor={theme.muted} overflow="hidden">
      <Box paddingX={1} flexDirection="column">
        {mode === "summary" && <SummaryView stats={stats} factSummaries={factSummaries} theme={theme} />}
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

function Label({ label, value, theme, valueColor }: {
  label: string; value: string; theme: Theme; valueColor?: string;
}) {
  return (
    <Box>
      <Text color={theme.muted}>{label} </Text>
      <Text color={valueColor ?? undefined} bold>{value}</Text>
    </Box>
  );
}

function ConfidenceBar({ value, theme }: { value: number; theme: Theme }) {
  const filled = Math.round(value * 10);
  const empty = 10 - filled;
  const color = value >= 0.7 ? theme.success : value >= 0.4 ? theme.highlight : theme.error;
  return (
    <Box>
      <Text color={theme.muted}>{glyph.confidence} </Text>
      <Text color={color}>{"█".repeat(filled)}</Text>
      <Text color={theme.muted}>{"░".repeat(empty)}</Text>
      <Text color={theme.muted}> {(value * 100).toFixed(0)}%</Text>
    </Box>
  );
}

function SummaryView({ stats, factSummaries, theme }: {
  stats?: StatsResult | null; factSummaries?: FactSummaryItem[]; theme: Theme;
}) {
  if (!stats) return <Text color={theme.muted}>Loading...</Text>;
  if (stats.totalFacts === 0) return <Text color={theme.muted}>{glyph.empty} No facts in this subtree.</Text>;

  const domains = Object.entries(stats.domainCounts)
    .sort((a, b) => b[1] - a[1])
    .slice(0, 10);
  const maxCount = domains.length > 0 ? domains[0][1] : 0;

  const facts = factSummaries?.filter((f) => f.summary) ?? [];

  return (
    <Box flexDirection="column">
      <Text color={theme.primary} bold>{glyph.bullet} Overview</Text>
      <Text> </Text>
      <Label label="  Facts" value={String(stats.totalFacts)} theme={theme} />
      <Box>
        <Text color={theme.muted}>  Confidence </Text>
        <ConfidenceBar value={stats.avgConfidence} theme={theme} />
      </Box>
      {domains.length > 0 && (
        <>
          <Text> </Text>
          <Text color={theme.primary} bold>{glyph.bullet} Domains</Text>
          <Text> </Text>
          {domains.map(([domain, count]) => {
            const barLen = maxCount > 0 ? Math.max(1, Math.round((count / maxCount) * 16)) : 1;
            return (
              <Box key={domain}>
                <Text color={theme.muted}>  </Text>
                <Text color={theme.secondary}>{"▓".repeat(barLen)}</Text>
                <Text color={theme.muted}>{"░".repeat(Math.max(0, 16 - barLen))}</Text>
                <Text color={theme.muted}> </Text>
                <Text>{domain}</Text>
                <Text color={theme.muted}> ({count})</Text>
              </Box>
            );
          })}
        </>
      )}
      {facts.length > 0 && (
        <>
          <Text> </Text>
          <Text color={theme.muted}>{box.horizontal.repeat(40)}</Text>
          <Text> </Text>
          <Text color={theme.primary} bold>{glyph.bullet} Facts</Text>
          <Text> </Text>
          {facts.map((f) => (
            <Box key={f.name} flexDirection="column">
              <Box>
                <Text color={theme.accent}>  {glyph.fact} </Text>
                <Text bold>{f.name}</Text>
              </Box>
              <Text color={theme.muted}>    {f.summary}</Text>
            </Box>
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
      <Text color={theme.highlight} bold>{title}</Text>
      <Text> </Text>
      {frontmatter && (
        <Box flexDirection="column" marginBottom={1}>
          <Box gap={2}>
            <ConfidenceBar value={frontmatter.confidence} theme={theme} />
            <Label label="sources:" value={String(frontmatter.sources)} theme={theme} />
          </Box>
          {frontmatter.domain.length > 0 && (
            <Box>
              <Text color={theme.muted}>{glyph.tag} </Text>
              {frontmatter.domain.map((d, i) => (
                <React.Fragment key={d}>
                  {i > 0 && <Text color={theme.muted}>, </Text>}
                  <Text color={theme.secondary}>{d}</Text>
                </React.Fragment>
              ))}
            </Box>
          )}
          {frontmatter.entities.length > 0 && (
            <Box>
              <Text color={theme.muted}>{glyph.bullet} </Text>
              {frontmatter.entities.map((e, i) => (
                <React.Fragment key={e}>
                  {i > 0 && <Text color={theme.muted}>, </Text>}
                  <Text color={theme.accent}>{e}</Text>
                </React.Fragment>
              ))}
            </Box>
          )}
          <Box paddingY={0}>
            <Text color={theme.muted}>{box.horizontal.repeat(40)}</Text>
          </Box>
        </Box>
      )}
      <Text wrap="wrap">{body}</Text>
      {frontmatter && frontmatter.refs.length > 0 && (
        <Box flexDirection="column" marginTop={1}>
          <Text color={theme.muted}>{box.horizontal.repeat(40)}</Text>
          <Text color={theme.primary} bold>{glyph.bullet} References</Text>
          {frontmatter.refs.map((r) => (
            <Box key={r}>
              <Text color={theme.muted}>  {glyph.arrow} </Text>
              <Text color={theme.secondary}>{r}</Text>
            </Box>
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
      <Text color={theme.primary} bold>{glyph.bullet} History</Text>
      <Text color={theme.muted}>  {file}</Text>
      <Text> </Text>
      {entries.length === 0 ? (
        <Text color={theme.muted}> {glyph.empty} No history found.</Text>
      ) : (
        entries.map((e, i) => {
          const isLast = i === entries.length - 1;
          const date = new Date(e.date);
          const dateStr = date.toLocaleDateString("en-US", {
            month: "short",
            day: "numeric",
            year: "numeric",
          });
          const timeStr = date.toLocaleTimeString("en-US", {
            hour: "2-digit",
            minute: "2-digit",
            hour12: false,
          });
          return (
            <Box key={e.commit} flexDirection="column">
              <Box>
                <Text color={theme.primary}>  {glyph.timelineDot} </Text>
                <Text color={theme.highlight}>{e.commit.slice(0, 7)}</Text>
                <Text color={theme.muted}> {dateStr} {timeStr}</Text>
              </Box>
              <Box>
                <Text color={theme.muted}>  {isLast ? " " : glyph.timelineLine} </Text>
                <Text wrap="wrap">{e.message}</Text>
              </Box>
              {!isLast && (
                <Box>
                  <Text color={theme.muted}>  {glyph.timelineLine}</Text>
                </Box>
              )}
            </Box>
          );
        })
      )}
      <Text> </Text>
      <Text color={theme.muted}>  Press h to go back</Text>
    </Box>
  );
}
