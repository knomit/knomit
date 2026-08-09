import type { Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import './markdown.css';

/* The shared ReactMarkdown configuration — plugins and component overrides.
 * Pairs with markdown.css, which supplies the `.k-prose` typography.
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
 * Both exports are module-level constants so they keep their identity across
 * renders.
 *
 * markdown.css is imported here rather than by a call site: `.k-prose` is only
 * meaningful for output this module configured, so the styles travel with it
 * and a third file cannot forget to pull them in.
 */

/* A minimal mdast shape — enough to walk links without taking a dependency on
 * @types/mdast, which we only reach transitively through remark. */
interface MdastNode {
  type: string;
  url?: string;
  value?: string;
  children?: MdastNode[];
  position?: { start: { offset?: number }; end: { offset?: number } };
}

/* remark-gfm autolinks a bare `www.foo.com` — and synthesizes the missing
 * scheme as `http://`, turning a scheme-less link the author wrote into a
 * plaintext-HTTP one. Both schemes are inventions here; https is the safer
 * invention.
 *
 * Scoped to synthesized links by two independent checks, because a scheme the
 * author wrote is theirs to keep:
 *
 *   - The URL must be exactly `http://` + the link's literal text. Someone who
 *     typed `http://www.foo.com` as a bare autolink carries the scheme in the
 *     text too, so the URL would have to be `http://http://www.foo.com`.
 *   - The node's source span must be exactly that text. An explicit
 *     `[www.foo.com](http://www.foo.com)` passes the first check — the text is
 *     the bare host and the destination is text-plus-scheme — but the author
 *     typed the destination out by hand, brackets and all, so its span is
 *     longer. No span means a node some other plugin synthesized; leave it. */
function upgradeWwwAutolinks(node: MdastNode): void {
  if (node.type === 'link' && node.children?.length === 1) {
    const [text] = node.children;
    const { start, end } = node.position ?? {};
    if (text.type === 'text' && text.value && /^www\./i.test(text.value)
      && node.url === `http://${text.value}`
      && start?.offset !== undefined && end?.offset !== undefined
      && end.offset - start.offset === text.value.length) {
      node.url = `https://${text.value}`;
    }
  }
  node.children?.forEach(upgradeWwwAutolinks);
}

function remarkHttpsWww() {
  return upgradeWwwAutolinks;
}

export const markdownPlugins = [remarkGfm, remarkHttpsWww];

/* Links in rendered markdown point outward, and autolinking means a fact that
 * merely mentions a URL now renders a live one. The app has no router, so an
 * in-place navigation is a full unload — app state gone, and in the Wails
 * desktop shell the webview lands on the site with no chrome to get back. Open
 * externals in a new tab and deny the opened page a window.opener handle: the
 * same contract the References rail in FactBody.tsx applies to its http refs.
 *
 * Only http(s) hrefs. GFM footnotes link within the document (`#user-content-fn-1`);
 * those must stay in-place or the backref opens a blank tab.
 *
 * target="_blank" is a browser answer. The desktop webview silently drops the
 * new-window request, so externalLinks.ts intercepts these clicks there and
 * hands the URL to the OS; the boundary it uses is this same http(s) test. */
export const markdownComponents: Components = {
  a({ href, node, ...props }) {
    // react-markdown hands every override the hast node it rendered from. It is
    // not a DOM attribute, so it has to come off before the rest is spread —
    // otherwise React 19 stamps every link with node="[object Object]".
    void node;
    return /^https?:\/\//i.test(href || '')
      ? <a href={href} target="_blank" rel="noopener noreferrer" {...props} />
      : <a href={href} {...props} />;
  },
};
