import remarkGfm from 'remark-gfm';

/* The remark plugin set every ReactMarkdown in the app renders with.
 *
 * Fact bodies are authored as GFM — knomit's own pack specs use comparison
 * tables and job-state facts list bare URLs — so plain CommonMark renders a
 * table as literal pipe text and leaves URLs unclickable.
 *
 * Deliberately NOT remark-breaks. It would put each URL of a bare-URL list on
 * its own line, but fact bodies are hard-wrapped at 80 columns, so turning
 * every soft break into a <br> shatters ordinary prose at the wrap points. A
 * list of URLs is a source-authoring problem (write them as a markdown list),
 * not a rendering one. See the guard test in FactBody.test.tsx.
 *
 * Module-level constant so the array keeps its identity across renders.
 */
export const markdownPlugins = [remarkGfm];
