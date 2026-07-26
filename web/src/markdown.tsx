import type { AnchorHTMLAttributes } from 'react';
import type { Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';

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
 */

/* A minimal mdast shape — enough to walk links without taking a dependency on
 * @types/mdast, which we only reach transitively through remark. */
interface MdastNode {
  type: string;
  url?: string;
  value?: string;
  children?: MdastNode[];
}

/* remark-gfm autolinks a bare `www.foo.com` — and synthesizes the missing
 * scheme as `http://`, turning a scheme-less link the author wrote into a
 * plaintext-HTTP one. Both schemes are inventions here; https is the safer
 * invention.
 *
 * Scoped to synthesized links only: the URL must be exactly `http://` + the
 * link's literal text. An author who deliberately typed `http://www.foo.com`
 * carries the scheme in the text too, so that link fails the check and is left
 * alone. */
function upgradeWwwAutolinks(node: MdastNode): void {
  if (node.type === 'link' && node.children?.length === 1) {
    const [text] = node.children;
    if (text.type === 'text' && text.value && /^www\./i.test(text.value)
      && node.url === `http://${text.value}`) {
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
 * those must stay in-place or the backref opens a blank tab. */
export const markdownComponents: Components = {
  a: ({ href, ...props }: AnchorHTMLAttributes<HTMLAnchorElement>) => (
    /^https?:\/\//i.test(href || '')
      ? <a href={href} target="_blank" rel="noopener noreferrer" {...props} />
      : <a href={href} {...props} />
  ),
};
