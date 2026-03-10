import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";
import { parseKnomitRef, type ParsedRef } from "./refs.js";
import { parseFact, type Frontmatter } from "../facts.js";
import type { StatsResult } from "../search-index.js";
import type { LogEntry } from "../git.js";
import type { ChildItem } from "./state.js";

export type SummaryChild = ChildItem;

export interface RightSelectableItem {
  type: "domain" | "entity" | "fact" | "ref" | "changed-file";
  label: string;
  path?: string;
  ref?: ParsedRef;
  changeStatus?: "added" | "modified" | "deleted";
}

export interface HistoricalData {
  content?: string;
  children?: SummaryChild[];
  changedFiles?: { added: string[]; modified: string[]; deleted: string[] };
  lineDiff?: Set<number>;
  entry: LogEntry;
  commitBody?: string;
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
  availableHeight?: number;
  historyTarget?: string;
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
  for (const r of frontmatter.refs) {
    const parsed = parseKnomitRef(r);
    items.push({ type: "ref", label: r, ref: parsed ?? undefined });
  }
  return items;
}

export function buildChangedFileItems(
  changedFiles: { added: string[]; modified: string[]; deleted: string[] } | undefined,
  historyTarget: string,
): RightSelectableItem[] {
  if (!changedFiles) return [];
  const items: RightSelectableItem[] = [];
  for (const f of changedFiles.added) {
    items.push({ type: "changed-file", label: f, path: `${historyTarget}/${f}`, changeStatus: "added" });
  }
  for (const f of changedFiles.modified) {
    items.push({ type: "changed-file", label: f, path: `${historyTarget}/${f}`, changeStatus: "modified" });
  }
  for (const f of changedFiles.deleted) {
    items.push({ type: "changed-file", label: f, path: `${historyTarget}/${f}`, changeStatus: "deleted" });
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
  focused, selectedIndex, onItemsChanged, historical, availableHeight, historyTarget,
}: RightPanelProps) {
  let selectableItems: RightSelectableItem[] = [];
  if (historical?.changedFiles) {
    selectableItems = buildChangedFileItems(historical.changedFiles, historyTarget ?? "");
  } else if (!historical) {
    if (mode === "summary") {
      selectableItems = buildSelectableItems(stats, summaryChildren);
    } else if (mode === "fact") {
      selectableItems = buildFactSelectableItems(factFrontmatter);
    }
  }

  React.useEffect(() => {
    onItemsChanged?.(selectableItems);
  }, [selectableItems.length, mode, !!historical]);

  const contentHeight = Math.max(3, (availableHeight ?? 24) - 3);

  return (
    <Box flexDirection="column" width="60%" paddingX={2} paddingTop={1} overflow="hidden" backgroundColor={theme.base}>
      <Box flexDirection="column" flexGrow={1} overflow="hidden">
        {historical ? (
          <HistoricalView data={historical} theme={theme} maxHeight={contentHeight} />
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
          <FactView title={factTitle ?? ""} body={factBody ?? ""} frontmatter={factFrontmatter} theme={theme} focused={focused} selectedIndex={selectedIndex} maxHeight={contentHeight} />
        ) : null}
      </Box>
    </Box>
  );
}

function HistoricalView({ data, theme, maxHeight }: { data: HistoricalData; theme: Theme; maxHeight: number }) {
  const { content, children, changedFiles, lineDiff, entry, commitBody } = data;
  const diff = changedFiles ? { added: new Set(changedFiles.added), modified: new Set(changedFiles.modified) } : undefined;
  const commitShort = entry.commit.slice(0, 7);
  const dateStr = new Date(entry.date).toLocaleDateString("en-US", {
    month: "short", day: "numeric", year: "numeric",
  });

  const isFact = content !== undefined && content !== "";
  let parsed: { title: string; body: string; frontmatter?: Frontmatter } | null = null;
  let bodyStartLine = 0;
  if (isFact) {
    try {
      parsed = parseFact(content);
      const fmMatch = content.match(/^---\n[\s\S]*?\n---\n/);
      bodyStartLine = fmMatch ? fmMatch[0].split("\n").length : 0;
    } catch {
      parsed = { title: "", body: content };
    }
  }

  const hasLineDiff = lineDiff && lineDiff.size > 0;
  const fmChanged = hasLineDiff && Array.from(lineDiff).some((n) => n > 0 && n < bodyStartLine);

  const commitBodyLines = commitBody ? commitBody.split("\n").length + 1 : 0;
  const headerLines = 4 + commitBodyLines + (parsed?.frontmatter ? 4 : 0);
  const bodyMaxLines = Math.max(1, maxHeight - headerLines);
  const bodyLines = parsed?.body.split("\n") ?? [];
  const truncatedBody = bodyLines.length > bodyMaxLines;
  const visibleBodyLines = bodyLines.slice(0, bodyMaxLines);

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
      {commitBody && (
        <Box marginTop={1}>
          <Text color={theme.dim} italic wrap="wrap">{commitBody}</Text>
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
              {visibleBodyLines.map((line, i) => {
                const fileLineNum = bodyStartLine + i;
                const changed = lineDiff!.has(fileLineNum);
                return (
                  <Box key={i}>
                    {changed && <Text color={theme.green} bold>+ </Text>}
                    <Text color={changed ? theme.green : undefined} wrap="wrap">{line}</Text>
                  </Box>
                );
              })}
              {truncatedBody && <Text color={theme.dim}>{glyph.bullet}{glyph.bullet}{glyph.bullet} {bodyLines.length - bodyMaxLines} more lines</Text>}
            </Box>
          ) : (
            <Box flexDirection="column">
              {visibleBodyLines.map((line, i) => (
                <Box key={i}><Text wrap="wrap">{line}</Text></Box>
              ))}
              {truncatedBody && <Text color={theme.dim}>{glyph.bullet}{glyph.bullet}{glyph.bullet} {bodyLines.length - bodyMaxLines} more lines</Text>}
            </Box>
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
          {changedFiles && (changedFiles.added.length > 0 || changedFiles.modified.length > 0 || changedFiles.deleted.length > 0) && (
            <Box flexDirection="column" marginTop={1}>
              <Text color={theme.dim}>{glyph.dashDivider} Changed files:</Text>
              {changedFiles.added.map((f) => (
                <Box key={f}>
                  <Text color={theme.green} bold>+ </Text>
                  <Text color={theme.green}>{f}</Text>
                </Box>
              ))}
              {changedFiles.modified.map((f) => (
                <Box key={f}>
                  <Text color={theme.yellow} bold>~ </Text>
                  <Text color={theme.yellow}>{f}</Text>
                </Box>
              ))}
              {changedFiles.deleted.map((f) => (
                <Box key={f}>
                  <Text color={theme.red} bold>- </Text>
                  <Text color={theme.red}>{f}</Text>
                </Box>
              ))}
            </Box>
          )}
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

function SummaryView({ stats, summaryChildren, theme, focused, selectedIndex }: {
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

function FactView({ title, body, frontmatter, theme, focused, selectedIndex, maxHeight }: {
  title: string; body: string; frontmatter?: Frontmatter; theme: Theme;
  focused?: boolean; selectedIndex?: number; maxHeight: number;
}) {
  const domainCount = frontmatter?.domain.length ?? 0;

  const headerLines = 2 + (frontmatter ? 3 + (frontmatter.domain.length > 0 ? 1 + frontmatter.domain.length : 0) + (frontmatter.entities.length > 0 ? 1 + frontmatter.entities.length : 0) + 1 : 0);
  const refsLines = frontmatter?.refs.length ? frontmatter.refs.length + 2 : 0;
  const bodyMaxLines = Math.max(1, maxHeight - headerLines - refsLines);
  const bodyLines = body.split("\n");
  const truncatedBody = bodyLines.length > bodyMaxLines;
  const visibleBodyLines = bodyLines.slice(0, bodyMaxLines);

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
      {visibleBodyLines.map((line, i) => (
        <Box key={i}><Text wrap="wrap">{line}</Text></Box>
      ))}
      {truncatedBody && <Text color={theme.dim}>{glyph.bullet}{glyph.bullet}{glyph.bullet} {bodyLines.length - bodyMaxLines} more lines</Text>}
      {frontmatter && frontmatter.refs.length > 0 && (
        <Box flexDirection="column" marginTop={1}>
          <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
          <Text color={theme.primary} bold>{glyph.bullet} References</Text>
          {frontmatter.refs.map((r, ri) => {
            const parsed = parseKnomitRef(r);
            const itemIdx = domainCount + (frontmatter.entities?.length ?? 0) + ri;
            const isActive = focused && selectedIndex === itemIdx;
            const isExternal = parsed?.external;
            return (
              <Box key={r}>
                <Text color={isActive ? theme.yellow : theme.dim}>
                  {isActive ? `  ${glyph.cursor} ` : "    "}
                </Text>
                <Text color={isActive ? theme.yellow : isExternal ? theme.dim : theme.secondary}>
                  {glyph.arrow} {r}
                </Text>
                {isExternal && <Text color={theme.dim}> (external)</Text>}
              </Box>
            );
          })}
        </Box>
      )}
    </Box>
  );
}
