import { parse as parseYaml, stringify as stringifyYaml } from "yaml";

export interface Frontmatter {
  domain: string[];
  confidence: number;
  sources: number;
  entities: string[];
  refs: string[];
}

export interface ParsedFact {
  frontmatter: Frontmatter;
  title: string;
  body: string;
}

export function parseFact(raw: string): ParsedFact {
  const match = raw.match(/^---\n([\s\S]*?)\n---\n([\s\S]*)$/);
  if (!match) throw new Error("Invalid fact file: no YAML frontmatter found");

  const yamlStr = match[1];
  const markdown = match[2].trim();

  const fm = parseYaml(yamlStr);
  if (!fm || typeof fm !== "object") throw new Error("Invalid YAML frontmatter");

  const frontmatter: Frontmatter = {
    domain: fm.domain ?? [],
    confidence: fm.confidence ?? 0,
    sources: fm.sources ?? 0,
    entities: fm.entities ?? [],
    refs: fm.refs ?? [],
  };

  // Extract title from first H1
  const titleMatch = markdown.match(/^# (.+)$/m);
  const title = titleMatch ? titleMatch[1] : "";

  // Body is everything after the title line
  const body = titleMatch
    ? markdown.slice(markdown.indexOf("\n", markdown.indexOf(titleMatch[0])) + 1).trim()
    : markdown;

  return { frontmatter, title, body };
}

export function serializeFact(
  frontmatter: Frontmatter,
  title: string,
  body: string
): string {
  const yaml = stringifyYaml(frontmatter, { lineWidth: 0 }).trim();
  return `---\n${yaml}\n---\n# ${title}\n\n${body}\n`;
}

export function mergeFrontmatter(
  existing: Frontmatter,
  updates: Partial<Omit<Frontmatter, "refs"> & { refs?: string[] }>
): Frontmatter {
  return {
    domain: updates.domain ?? existing.domain,
    confidence: updates.confidence ?? existing.confidence,
    sources: updates.sources ?? existing.sources,
    entities: updates.entities ?? existing.entities,
    refs: updates.refs ? [...existing.refs, ...updates.refs] : existing.refs,
  };
}
