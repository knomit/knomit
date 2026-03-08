import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";
import { parseFact, type Frontmatter } from "../facts.js";
import type { StatsResult } from "../search-index.js";
import type { LogEntry } from "../git.js";
import type { ChildItem } from "./state.js";

export type SummaryChild = ChildItem;

export interface RightSelectableItem {
  type: "domain" | "entity" | "fact";
  label: string;
  path?: string;
}

export interface HistoricalData {
  content?: string;
  children?: SummaryChild[];
  diff?: { added: Set<string>; modified: Set<string> };
  lineDiff?: Set<number>;
  entry: LogEntry;
}

interface RightPanelProps {
  mode: "summary" | "fact";
  theme: Theme;
  stats?: StatsResult | null;
  summaryChildren?: SummaryChild[];
  factTitle?: string;
  factBody?: string;
  factFrontmatter?: Frontmatter;
  focused?: boolean;
  selectedIndex?: number;
  onItemsChanged?: (items: RightSelectableItem[]) => void;
  historical?: HistoricalData;
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
  mode, theme, stats, summaryChildren, factTitle, factBody, factFrontmatter,
  focused, selectedIndex, onItemsChanged, historical,
}: RightPanelProps) {
  let selectableItems: RightSelectableItem[] = [];
  if (!historical) {
    if (mode === "summary") {
      selectableItems = buildSelectableItems(stats, summaryChildren);
    } else if (mode === "fact") {
      selectableItems = buildFactSelectableItems(factFrontmatter);
    }
  }

  React.useEffect(() => {
    onItemsChanged?.(selectableItems);
  }, [selectableItems.length, mode, !!historical]);

  return (
    <Box flexDirection="column" width="60%" paddingX={2} paddingTop={1} overflow="hidden" backgroundColor={theme.base}>
      <Box flexDirection="column" flexGrow={1} overflow="hidden">
        {historical ? (
          <HistoricalView data={historical} theme={theme} />
        ) : mode === "summary" ? (
          <SummaryView
            stats={stats}
            summaryChildren={summaryChildren}
            theme={theme}
            focused={focused}
            selectedIndex={selectedIndex}
            selectableItems={selectableItems}
          />
        ) : mode === "fact" ? (
          <FactView title={factTitle ?? ""} body={factBody ?? ""} frontmatter={factFrontmatter} theme={theme} focused={focused} selectedIndex={selectedIndex} />
        ) : null}
      </Box>
    </Box>
  );
}

function HistoricalView({ data, theme }: { data: HistoricalData; theme: Theme }) {
  const { content, children, diff, lineDiff, entry } = data;
  const commitShort = entry.commit.slice(0, 7);
  const dateStr = new Date(entry.date).toLocaleDateString("en-US", {
    month: "short", day: "numeric", year: "numeric",
  });

  const isFact = content !== undefined && content !== "";
  let parsed: { title: string; body: string; frontmatter?: Frontmatter } | null = null;
  // Track which lines in the raw file correspond to the body, for diff highlighting
  let bodyStartLine = 0;
  if (isFact) {
    try {
      parsed = parseFact(content);
      // Body starts after "---\n<yaml>\n---\n" — count lines before body
      const fmMatch = content.match(/^---\n[\s\S]*?\n---\n/);
      bodyStartLine = fmMatch ? fmMatch[0].split("\n").length : 0;
    } catch {
      parsed = { title: "", body: content };
    }
  }

  const hasLineDiff = lineDiff && lineDiff.size > 0;

  // Check if frontmatter lines were changed
  const fmChanged = hasLineDiff && Array.from(lineDiff).some((n) => n > 0 && n < bodyStartLine);

  return (
    <Box flexDirection="column">
      <Box>
        <Text color={theme.primary} bold>{glyph.bullet} Snapshot</Text>
        <Text color={theme.dim}> at </Text>
        <Text color={theme.yellow}>{commitShort}</Text>
        <Text color={theme.dim}> {dateStr}</Text>
      </Box>
      <Box>
        <ActionBadge message={entry.message} theme={theme} />
      </Box>
      {entry.episode && (
        <Box>
          <Text backgroundColor={theme.secondary} color={theme.dark} bold> {entry.episode} </Text>
        </Box>
      )}
      <Text> </Text>
      {parsed ? (
        <Box flexDirection="column">
          <Text color={theme.yellow} bold>{parsed.title}</Text>
          <Text> </Text>
          {parsed.frontmatter && (
            <Box flexDirection="column" marginBottom={1}>
              <Box gap={2}>
                <ConfidenceBar value={parsed.frontmatter.confidence} theme={theme} />
                <Label label="sources:" value={String(parsed.frontmatter.sources)} theme={theme} />
                {fmChanged && <Text color={theme.green} bold>~</Text>}
              </Box>
              {parsed.frontmatter.domain.length > 0 && (
                <Box>
                  <Text color={theme.dim}>{glyph.tag} </Text>
                  <Text color={fmChanged ? theme.green : theme.secondary}>{parsed.frontmatter.domain.join(", ")}</Text>
                </Box>
              )}
              {parsed.frontmatter.entities.length > 0 && (
                <Box>
                  <Text color={theme.dim}>{glyph.bullet} </Text>
                  <Text color={fmChanged ? theme.green : theme.accent}>{parsed.frontmatter.entities.join(", ")}</Text>
                </Box>
              )}
              <Box>
                <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
              </Box>
            </Box>
          )}
          {hasLineDiff ? (
            <Box flexDirection="column">
              {parsed.body.split("\n").map((line, i) => {
                const fileLineNum = bodyStartLine + i;
                const changed = lineDiff.has(fileLineNum);
                return (
                  <Box key={i}>
                    {changed && <Text color={theme.green} bold>+ </Text>}
                    <Text color={changed ? theme.green : undefined} wrap="wrap">{line}</Text>
                  </Box>
                );
              })}
            </Box>
          ) : (
            <Text wrap="wrap">{parsed.body}</Text>
          )}
        </Box>
      ) : children && children.length > 0 ? (
        <Box flexDirection="column">
          {children.map((c) => {
            const isNew = diff?.added.has(c.name);
            const isMod = diff?.modified.has(c.name);
            const badge = isNew ? " +" : isMod ? " ~" : "";
            const badgeColor = isNew ? theme.green : isMod ? theme.yellow : undefined;
            const nameColor = isNew ? theme.green : isMod ? theme.yellow : undefined;
            return (
              <Box key={c.name}>
                <Text color={c.type === "world" ? theme.secondary : theme.accent}>
                  {c.type === "world" ? glyph.world : glyph.fact}{" "}
                </Text>
                <Text color={nameColor}>{c.name}</Text>
                {badge && <Text color={badgeColor} bold>{badge}</Text>}
              </Box>
            );
          })}
        </Box>
      ) : (
        <Text color={theme.dim}>{glyph.empty} No content at this commit.</Text>
      )}
    </Box>
  );
}

function Label({ label, value, theme, valueColor }: {
  label: string; value: string; theme: Theme; valueColor?: string;
}) {
  return (
    <Box>
      <Text color={theme.dim}>{label} </Text>
      <Text color={valueColor} bold>{value}</Text>
    </Box>
  );
}

function confidenceColor(value: number, theme: Theme): string {
  if (value >= 0.7) return theme.green;
  if (value >= 0.4) return theme.yellow;
  return theme.red;
}

function ConfidenceBar({ value, theme }: { value: number; theme: Theme }) {
  const filled = Math.round(value * 10);
  const empty = 10 - filled;
  const color = confidenceColor(value, theme);
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
          <Box>
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

