import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";
import { glyph } from "./theme.js";
import type { Frontmatter } from "../facts.js";
import type { StatsResult } from "../search-index.js";
import type { LogEntry } from "../git.js";

export interface SummaryChild {
  name: string;
  type: "world" | "fact";
  summary?: string;
}

export interface RightSelectableItem {
  type: "domain" | "entity" | "fact";
  label: string;
  path?: string;
}

interface RightPanelProps {
  mode: "summary" | "fact" | "history";
  theme: Theme;
  stats?: StatsResult | null;
  summaryChildren?: SummaryChild[];
  factTitle?: string;
  factBody?: string;
  factFrontmatter?: Frontmatter;
  history?: LogEntry[];
  historyFile?: string;
  focused?: boolean;
  selectedIndex?: number;
  onItemsChanged?: (items: RightSelectableItem[]) => void;
}

export function buildFactSelectableItems(
  frontmatter: Frontmatter | undefined,
): RightSelectableItem[] {
  const items: RightSelectableItem[] = [];
  if (!frontmatter) return items;
  for (const d of frontmatter.domain) {
    items.push({ type: "domain", label: d });
  }
  for (const e of frontmatter.entities) {
    items.push({ type: "entity", label: e });
  }
  return items;
}

export function buildSelectableItems(
  stats: StatsResult | null | undefined,
  summaryChildren: SummaryChild[] | undefined,
): RightSelectableItem[] {
  const items: RightSelectableItem[] = [];
  if (stats) {
    const domains = Object.entries(stats.domainCounts)
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10);
    for (const [domain] of domains) {
      items.push({ type: "domain", label: domain });
    }
    const entities = Object.entries(stats.entityCounts)
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10);
    for (const [entity] of entities) {
      items.push({ type: "entity", label: entity });
    }
  }
  const facts = summaryChildren?.filter((c) => c.type === "fact" && c.summary) ?? [];
  for (const f of facts) {
    items.push({ type: "fact", label: f.name, path: f.name });
  }
  return items;
}

export function RightPanel({
  mode, theme, stats, summaryChildren, factTitle, factBody, factFrontmatter, history, historyFile,
  focused, selectedIndex, onItemsChanged,
}: RightPanelProps) {
  const selectableItems = mode === "summary"
    ? buildSelectableItems(stats, summaryChildren)
    : mode === "fact"
      ? buildFactSelectableItems(factFrontmatter)
      : [];

  React.useEffect(() => {
    onItemsChanged?.(selectableItems);
  }, [selectableItems.length, mode]);

  return (
    <Box flexDirection="column" width="60%" paddingX={2} paddingTop={1} overflow="hidden" backgroundColor={theme.base}>
      <Box flexDirection="column" flexGrow={1} overflow="hidden">
        {mode === "summary" && (
          <SummaryView
            stats={stats}
            summaryChildren={summaryChildren}
            theme={theme}
            focused={focused}
            selectedIndex={selectedIndex}
            selectableItems={selectableItems}
          />
        )}
        {mode === "fact" && (
          <FactView title={factTitle ?? ""} body={factBody ?? ""} frontmatter={factFrontmatter} theme={theme} focused={focused} selectedIndex={selectedIndex} />
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
      <Text color={theme.dim}>{label} </Text>
      <Text color={valueColor ?? undefined} bold>{value}</Text>
    </Box>
  );
}

function ConfidenceBar({ value, theme }: { value: number; theme: Theme }) {
  const filled = Math.round(value * 10);
  const empty = 10 - filled;
  const color = value >= 0.7 ? theme.green : value >= 0.4 ? theme.yellow : theme.red;
  return (
    <Box>
      <Text color={theme.dim}>{glyph.confidence} </Text>
      <Text color={color}>{"█".repeat(filled)}</Text>
      <Text color={theme.surface}>{"░".repeat(empty)}</Text>
      <Text color={theme.dim}> {(value * 100).toFixed(0)}%</Text>
    </Box>
  );
}

function ActionBadge({ message, theme }: { message: string; theme: Theme }) {
  const lower = message.toLowerCase();
  let action = "";
  let bg = theme.surface;
  if (lower.startsWith("learn")) { action = "learn"; bg = theme.green; }
  else if (lower.startsWith("update")) { action = "update"; bg = theme.blue; }
  else if (lower.startsWith("delete") || lower.startsWith("remove")) { action = "delete"; bg = theme.red; }

  if (!action) return <Text wrap="wrap">{message}</Text>;

  const rest = message.slice(action.length).trimStart();
  return (
    <Box>
      <Text backgroundColor={bg} color={theme.dark}> {action} </Text>
      {rest && <Text> {rest}</Text>}
    </Box>
  );
}

function SummaryView({ stats, summaryChildren, theme, focused, selectedIndex, selectableItems }: {
  stats?: StatsResult | null; summaryChildren?: SummaryChild[]; theme: Theme;
  focused?: boolean; selectedIndex?: number; selectableItems?: RightSelectableItem[];
}) {
  if (!stats) return <Text color={theme.dim}>Loading...</Text>;
  if (stats.totalFacts === 0) return <Text color={theme.dim}>{glyph.empty} No facts in this subtree.</Text>;

  const domains = Object.entries(stats.domainCounts)
    .sort((a, b) => b[1] - a[1])
    .slice(0, 10);
  const maxCount = domains.length > 0 ? domains[0][1] : 0;

  const entities = stats.entityCounts
    ? Object.entries(stats.entityCounts)
        .sort((a, b) => b[1] - a[1])
        .slice(0, 10)
    : [];

  const worlds = summaryChildren?.filter((c) => c.type === "world") ?? [];
  const facts = summaryChildren?.filter((c) => c.type === "fact" && c.summary) ?? [];

  return (
    <Box flexDirection="column">
      <Text color={theme.primary} bold>{glyph.bullet} Overview</Text>
      <Text> </Text>
      <Label label="Facts" value={String(stats.totalFacts)} theme={theme} />
      <Box>
        <Text color={theme.dim}>Confidence </Text>
        <ConfidenceBar value={stats.avgConfidence} theme={theme} />
      </Box>
      {domains.length > 0 && (
        <>
          <Text> </Text>
          <Text color={theme.primary} bold>{glyph.bullet} Domains</Text>
          <Text> </Text>
          {domains.map(([domain, count], di) => {
            const barLen = maxCount > 0 ? Math.max(1, Math.round((count / maxCount) * 16)) : 1;
            const isActive = focused && selectedIndex === di;
            return (
              <Box key={domain}>
                <Text color={isActive ? theme.yellow : theme.dim}>
                  {isActive ? `${glyph.cursor} ` : "  "}
                </Text>
                <Text color={isActive ? theme.yellow : theme.secondary}>{"▓".repeat(barLen)}</Text>
                <Text color={theme.surface}>{"░".repeat(Math.max(0, 16 - barLen))}</Text>
                <Text color={isActive ? theme.yellow : undefined}> {domain}</Text>
                <Text color={theme.dim}> ({count})</Text>
              </Box>
            );
          })}
        </>
      )}
      {entities.length > 0 && (
        <>
          <Text> </Text>
          <Text color={theme.primary} bold>{glyph.bullet} Entities</Text>
          <Text> </Text>
          {entities.map(([entity, count], ei) => {
            const itemIdx = domains.length + ei;
            const isActive = focused && selectedIndex === itemIdx;
            return (
              <Box key={entity}>
                <Text color={isActive ? theme.yellow : theme.dim}>
                  {isActive ? `${glyph.cursor} ` : "  "}
                </Text>
                <Text color={isActive ? theme.yellow : theme.accent}>{entity}</Text>
                <Text color={theme.dim}> ({count})</Text>
              </Box>
            );
          })}
        </>
      )}
      {worlds.length > 0 && (
        <>
          <Text> </Text>
          <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
          <Text> </Text>
          <Text color={theme.primary} bold>{glyph.bullet} Ontology</Text>
          <Text> </Text>
          {worlds.map((w) => (
            <Box key={w.name}>
              <Text color={theme.secondary}>  {glyph.world} </Text>
              <Text bold>{w.name}</Text>
            </Box>
          ))}
        </>
      )}
      {facts.length > 0 && (
        <>
          <Text> </Text>
          <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
          <Text> </Text>
          <Text color={theme.primary} bold>{glyph.bullet} Facts</Text>
          <Text> </Text>
          {facts.map((f, fi) => {
            const itemIdx = domains.length + entities.length + fi;
            const isActive = focused && selectedIndex === itemIdx;
            return (
              <Box key={f.name} flexDirection="column">
                <Box>
                  <Text color={isActive ? theme.yellow : theme.dim}>
                    {isActive ? `${glyph.cursor} ` : "  "}
                  </Text>
                  <Text color={isActive ? theme.yellow : theme.accent}>{glyph.fact} </Text>
                  <Text color={isActive ? theme.yellow : undefined} bold>{f.name}</Text>
                </Box>
                <Text color={theme.dim}>    {f.summary}</Text>
              </Box>
            );
          })}
        </>
      )}
    </Box>
  );
}

function FactView({ title, body, frontmatter, theme, focused, selectedIndex }: {
  title: string; body: string; frontmatter?: Frontmatter; theme: Theme;
  focused?: boolean; selectedIndex?: number;
}) {
  const domainCount = frontmatter?.domain.length ?? 0;
  return (
    <Box flexDirection="column">
      <Text color={theme.yellow} bold>{title}</Text>
      <Text> </Text>
      {frontmatter && (
        <Box flexDirection="column" marginBottom={1}>
          <Box gap={2}>
            <ConfidenceBar value={frontmatter.confidence} theme={theme} />
            <Label label="sources:" value={String(frontmatter.sources)} theme={theme} />
          </Box>
          {frontmatter.domain.length > 0 && (
            <Box flexDirection="column">
              <Text color={theme.dim}>{glyph.tag} Domains</Text>
              {frontmatter.domain.map((d, i) => {
                const isActive = focused && selectedIndex === i;
                return (
                  <Box key={d}>
                    <Text color={isActive ? theme.yellow : theme.dim}>
                      {isActive ? `  ${glyph.cursor} ` : "    "}
                    </Text>
                    <Text color={isActive ? theme.yellow : theme.secondary}>{d}</Text>
                  </Box>
                );
              })}
            </Box>
          )}
          {frontmatter.entities.length > 0 && (
            <Box flexDirection="column">
              <Text color={theme.dim}>{glyph.bullet} Entities</Text>
              {frontmatter.entities.map((e, i) => {
                const isActive = focused && selectedIndex === domainCount + i;
                return (
                  <Box key={e}>
                    <Text color={isActive ? theme.yellow : theme.dim}>
                      {isActive ? `  ${glyph.cursor} ` : "    "}
                    </Text>
                    <Text color={isActive ? theme.yellow : theme.accent}>{e}</Text>
                  </Box>
                );
              })}
            </Box>
          )}
          <Box paddingY={0}>
            <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
          </Box>
        </Box>
      )}
      <Text wrap="wrap">{body}</Text>
      {frontmatter && frontmatter.refs.length > 0 && (
        <Box flexDirection="column" marginTop={1}>
          <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
          <Text color={theme.primary} bold>{glyph.bullet} References</Text>
          {frontmatter.refs.map((r) => (
            <Box key={r}>
              <Text color={theme.dim}>{glyph.arrow} </Text>
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
      <Text color={theme.dim}>{file}</Text>
      <Text> </Text>
      {entries.length === 0 ? (
        <Text color={theme.dim}>{glyph.empty} No history found.</Text>
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
                <Text color={theme.primary}>{glyph.timelineDot} </Text>
                <Text color={theme.yellow}>{e.commit.slice(0, 7)}</Text>
                <Text color={theme.dim}> {dateStr} {timeStr}</Text>
              </Box>
              <Box>
                <Text color={theme.dim}>{isLast ? " " : glyph.timelineLine} </Text>
                <ActionBadge message={e.message} theme={theme} />
              </Box>
              {!isLast && (
                <Box>
                  <Text color={theme.dim}>{glyph.timelineLine}</Text>
                </Box>
              )}
            </Box>
          );
        })
      )}
      <Text> </Text>
      <Text color={theme.dim}>Press h to go back</Text>
    </Box>
  );
}
