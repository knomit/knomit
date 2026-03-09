import type { GitRepo } from "./git";
import type { SearchIndex } from "./search-index";
import { serializeFact, parseFact, mergeFrontmatter } from "./facts";

export interface FactData {
  path: string;
  title: string;
  body: string;
  domain: string[];
  confidence: number;
  sources: number;
  entities: string[];
  refs: string[];
}

/** Create or overwrite a fact file and commit. Returns the commit hash. */
export async function commitFact(
  repo: GitRepo,
  fact: FactData,
  searchIndex?: SearchIndex,
  reason?: string
): Promise<string> {
  let factPath = fact.path;
  if (!factPath.startsWith("worlds/")) factPath = `worlds/${factPath}`;
  if (!factPath.endsWith(".md")) factPath = `${factPath}.md`;

  const content = serializeFact(
    {
      domain: fact.domain,
      confidence: fact.confidence,
      sources: fact.sources,
      entities: fact.entities,
      refs: fact.refs,
    },
    fact.title,
    fact.body
  );

  const msg = reason ? `learn: ${fact.title}\n\n${reason}` : `learn: ${fact.title}`;
  const hash = await repo.commit(
    [{ path: factPath, content }],
    msg
  );

  await searchIndex?.upsert(factPath, {
    title: fact.title,
    body: fact.body,
    domain: fact.domain,
    entities: fact.entities,
    confidence: fact.confidence,
    sources: fact.sources,
    refs: fact.refs,
    commitHash: hash,
  });

  return hash;
}

/** Delete a fact file and commit. Returns the commit hash. */
export async function deleteFact(
  repo: GitRepo,
  file: string,
  momentName: string,
  searchIndex?: SearchIndex,
  reason?: string
): Promise<string> {
  const msg = reason
    ? `forget(${momentName}): ${file}\n\n${reason}`
    : `forget(${momentName}): ${file}`;
  const hash = await repo.deleteFile(file, msg);
  searchIndex?.remove(file);
  return hash;
}

/** Update a fact's frontmatter/content and commit. Returns the commit hash. */
export async function updateFact(
  repo: GitRepo,
  file: string,
  updates: {
    confidence?: number;
    sources?: number;
    body?: string;
    title?: string;
    refs?: string[];
    domain?: string[];
    entities?: string[];
  },
  searchIndex?: SearchIndex,
  reason?: string
): Promise<string> {
  const content = await repo.readFile(file);
  const parsed = parseFact(content);
  const newFrontmatter = mergeFrontmatter(parsed.frontmatter, updates);
  const newTitle = updates.title ?? parsed.title;
  const newBody = updates.body ?? parsed.body;
  const serialized = serializeFact(newFrontmatter, newTitle, newBody);

  const msg = reason ? `update: ${newTitle}\n\n${reason}` : `update: ${newTitle}`;
  const hash = await repo.commit(
    [{ path: file, content: serialized }],
    msg
  );

  await searchIndex?.upsert(file, {
    title: newTitle,
    body: newBody,
    domain: newFrontmatter.domain,
    entities: newFrontmatter.entities,
    confidence: newFrontmatter.confidence,
    sources: newFrontmatter.sources,
    refs: newFrontmatter.refs,
    commitHash: hash,
  });

  return hash;
}
