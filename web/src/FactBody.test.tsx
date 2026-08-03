import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { FactBody } from './FactBody';
import type { Fact } from './api';

const baseFact: Fact = {
  path: 'kb/x.md',
  title: 'X title',
  type: 'concept',
  body: 'Hello **world**',
  domain: ['ai', 'distribution'],
  confidence: 0.87,
  sources: 3,
  entities: ['Anthropic'],
  // Refs arrive from the server pre-classified; the client never re-derives.
  refs: [
    { raw: 'https://example.com/paper', kind: 'url' },
    { raw: 'kb/local-ref.md', kind: 'fact', path: 'kb/local-ref.md' },
  ],
};

describe('FactBody', () => {
  it('renders type badge, stat boxes, markdown body, domains, entities', () => {
    render(<FactBody fact={baseFact} dispatch={vi.fn()} readOnly={false} />);

    expect(screen.getByTestId('fact-type-badge')).toBeInTheDocument();
    expect(screen.getByText('0.87')).toBeInTheDocument();
    expect(screen.getByText('3')).toBeInTheDocument();
    expect(screen.getByText('ai')).toBeInTheDocument();
    expect(screen.getByText('distribution')).toBeInTheDocument();
    expect(screen.getByText('Anthropic')).toBeInTheDocument();
    expect(screen.getByText('world').tagName.toLowerCase()).toBe('strong');
  });

  it('readOnly=true: tag clicks do not dispatch', () => {
    const dispatch = vi.fn();
    render(<FactBody fact={baseFact} dispatch={dispatch} readOnly={true} />);

    fireEvent.click(screen.getByText('ai'));
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('readOnly=false: tag clicks dispatch ADD_FILTER', () => {
    const dispatch = vi.fn();
    render(<FactBody fact={baseFact} dispatch={dispatch} readOnly={false} />);

    fireEvent.click(screen.getByText('Anthropic'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER',
      chip: { category: 'entity', value: 'Anthropic' },
    });
  });

  it('clicking a local ref invokes onRefClick with the ref path', () => {
    const onRefClick = vi.fn();
    render(<FactBody fact={baseFact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    fireEvent.click(screen.getByText(/kb\/local-ref\.md/));
    expect(onRefClick).toHaveBeenCalledWith('kb/local-ref.md');
  });

  it('without onRefClick, local refs render as inert text and do not throw on click', () => {
    render(<FactBody fact={baseFact} dispatch={vi.fn()} readOnly={false} />);

    const localRef = screen.getByText(/kb\/local-ref\.md/);
    expect(localRef).toBeInTheDocument();
    fireEvent.click(localRef);
  });

  it('readOnly=true with onRefClick: local refs are still clickable (hop is read-only)', () => {
    const onRefClick = vi.fn();
    render(<FactBody fact={baseFact} dispatch={vi.fn()} readOnly={true} onRefClick={onRefClick} />);

    fireEvent.click(screen.getByText(/kb\/local-ref\.md/));
    expect(onRefClick).toHaveBeenCalledWith('kb/local-ref.md');
  });

  it('external (http) refs always render as anchor tags regardless of onRefClick', () => {
    const onRefClick = vi.fn();
    render(<FactBody fact={baseFact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    const ext = screen.getByText(/example\.com\/paper/);
    expect(ext.tagName.toLowerCase()).toBe('a');
    expect(ext).toHaveAttribute('href', 'https://example.com/paper');
  });

  // Fact bodies are authored as GFM — knomit's own pack specs use comparison
  // tables, and job-state facts list bare URLs. Without remark-gfm those render
  // as literal pipe-soup and unclickable text.
  describe('GFM', () => {
    it('renders a GFM table as a real table, not literal pipe text', () => {
      const body = [
        '| Too generic | Specific enough |',
        '|---|---|',
        '| Use the orchestrator-worker pattern. | Orchestrator-worker buys context isolation. |',
      ].join('\n');
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      const table = screen.getByTestId('fact-body').querySelector('table');
      expect(table).not.toBeNull();
      expect(table!.querySelectorAll('th')).toHaveLength(2);
      expect(table!.querySelectorAll('tbody tr')).toHaveLength(1);
      expect(screen.getByText('Too generic').tagName.toLowerCase()).toBe('th');
      // The raw delimiter row must not survive as visible text.
      expect(screen.getByTestId('fact-body').textContent).not.toContain('|---|');
    });

    it('autolinks bare URLs so job-state source lists are clickable', () => {
      const body = 'https://www.anthropic.com/engineering\nhttps://modelcontextprotocol.io/specification/versioning';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      const links = screen.getByTestId('fact-body').querySelectorAll('a');
      expect(links).toHaveLength(2);
      expect(links[0]).toHaveAttribute('href', 'https://www.anthropic.com/engineering');
      expect(links[1]).toHaveAttribute('href', 'https://modelcontextprotocol.io/specification/versioning');
    });

    // Autolinking means a fact that merely mentions a URL renders a live one.
    // The app has no router, so an in-place navigation is a full unload.
    it('opens external links in a new tab with no window.opener handle', () => {
      const body = 'see https://example.com/x and [a link](https://example.com/y)';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      const links = screen.getByTestId('fact-body').querySelectorAll('a');
      expect(links).toHaveLength(2);
      for (const a of links) {
        expect(a).toHaveAttribute('target', '_blank');
        expect(a).toHaveAttribute('rel', 'noopener noreferrer');
      }
    });

    // react-markdown hands every component override the hast node it rendered
    // from. It is not a DOM attribute, so spreading the leftover props onto the
    // <a> stamps every link with node="[object Object]".
    it('does not leak the hast node onto the rendered anchor', () => {
      const body = 'see https://example.com/x and [a link](/local) and a note.[^1]\n\n[^1]: n';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      const links = screen.getByTestId('fact-body').querySelectorAll('a');
      expect(links.length).toBeGreaterThan(2);
      for (const a of links) expect(a).not.toHaveAttribute('node');
    });

    // GFM footnote refs and backrefs link within the document; sending those to
    // a new tab would open a blank one.
    it('leaves in-document footnote links in place', () => {
      const body = 'A claim.[^1]\n\n[^1]: the note';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      const links = screen.getByTestId('fact-body').querySelectorAll('a');
      expect(links.length).toBeGreaterThan(0);
      for (const a of links) {
        expect(a.getAttribute('href')).toMatch(/^#/);
        expect(a).not.toHaveAttribute('target');
      }
    });

    // remark-gfm synthesizes `http://` for a scheme-less `www.` literal. Both
    // schemes are inventions; https is the safer one.
    it('autolinks a bare www. host as https, not plaintext http', () => {
      render(<FactBody fact={{ ...baseFact, body: 'see www.example.com today' }} dispatch={vi.fn()} readOnly={false} />);

      expect(screen.getByTestId('fact-body').querySelector('a'))
        .toHaveAttribute('href', 'https://www.example.com');
    });

    it('does not rewrite a scheme the author wrote explicitly', () => {
      render(<FactBody fact={{ ...baseFact, body: 'see http://www.example.com today' }} dispatch={vi.fn()} readOnly={false} />);

      expect(screen.getByTestId('fact-body').querySelector('a'))
        .toHaveAttribute('href', 'http://www.example.com');
    });

    // An explicit [text](dest) is not a synthesized link: the author typed the
    // destination out by hand, so the scheme is theirs even when the link text
    // happens to be the bare host.
    it('does not rewrite an explicit link whose text happens to be the www. host', () => {
      render(<FactBody fact={{ ...baseFact, body: '[www.example.com](http://www.example.com)' }} dispatch={vi.fn()} readOnly={false} />);

      expect(screen.getByTestId('fact-body').querySelector('a'))
        .toHaveAttribute('href', 'http://www.example.com');
    });

    // Nothing in the app defines `sr-only`; markdown.css does, so the GFM
    // footnote label stays out of the rendered prose.
    it('marks the footnote label sr-only so it does not render as a heading', () => {
      const body = 'A claim.[^1]\n\n[^1]: the note';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      const label = screen.getByTestId('fact-body').querySelector('#footnote-label');
      expect(label).not.toBeNull();
      expect(label).toHaveClass('sr-only');
      // With the label hidden, the section's own class is the only hook left
      // for the rule that separates the notes from the prose.
      expect(screen.getByTestId('fact-body').querySelector('section.footnotes')).not.toBeNull();
    });

    it('renders strikethrough and task lists', () => {
      const body = '~~retracted~~\n\n- [x] done\n- [ ] pending';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      expect(screen.getByText('retracted').tagName.toLowerCase()).toBe('del');
      const boxes = screen.getByTestId('fact-body').querySelectorAll('input[type="checkbox"]');
      expect(boxes).toHaveLength(2);
      expect((boxes[0] as HTMLInputElement).checked).toBe(true);
      expect((boxes[1] as HTMLInputElement).checked).toBe(false);
      // markdown.css drops the marker via `.task-list-item`, so the class that
      // remark-gfm tags these with is load-bearing.
      expect(screen.getByTestId('fact-body').querySelectorAll('li.task-list-item')).toHaveLength(2);
    });

    it('does NOT turn soft line breaks into hard breaks (prose is hard-wrapped at 80 cols)', () => {
      const body = 'The standing rules for this knowledge pack: what belongs in it, how facts are\nwritten, and where they go.';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      expect(screen.getByTestId('fact-body').querySelectorAll('br')).toHaveLength(0);
    });
  });

  it('marks the body with the prose class so markdown escapes the global CSS reset', () => {
    render(<FactBody fact={baseFact} dispatch={vi.fn()} readOnly={false} />);

    expect(screen.getByTestId('fact-body')).toHaveClass('k-prose');
  });

  it('src:// refs render as inert (browser cannot open a src: protocol, not a knomit fact path)', () => {
    const fact: Fact = { ...baseFact, refs: [{ raw: 'src://knomit/internal/store/service.go@cfef409', kind: 'source_code' }] };
    const onRefClick = vi.fn();
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    const el = screen.getByText(/src:\/\/knomit\/internal\/store\/service\.go/);
    expect(el.tagName.toLowerCase()).toBe('span');
    expect(el).not.toHaveAttribute('href');
    // Labelled, so a reader can tell a citation from a broken fact ref.
    expect(screen.getByText(/\(source\)/)).toBeInTheDocument();
    fireEvent.click(el);
    expect(onRefClick).not.toHaveBeenCalled();
  });

  it('file:/// refs render as inert (not a knomit fact path)', () => {
    const fact: Fact = { ...baseFact, refs: [{ raw: 'file:///etc/hosts', kind: 'url' }] };
    const onRefClick = vi.fn();
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    const el = screen.getByText(/file:\/\/\/etc\/hosts/);
    expect(el.tagName.toLowerCase()).toBe('span');
    expect(el).not.toHaveAttribute('href');
    fireEvent.click(el);
    expect(onRefClick).not.toHaveBeenCalled();
  });

  // The bug this replaced: a cross-repo ref ends in ".md", so the old server
  // rule sent it to the resolver, found no local fact by that literal name, and
  // reported it BROKEN. It must read as another repo, and must not be clickable.
  it('a ref to another repo reads as another repo, not as broken', () => {
    const onRefClick = vi.fn();
    const fact: Fact = { ...baseFact, refs: [{ raw: 'kb://7b4887ce51d9/kb/z.md', kind: 'foreign' }] };
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    expect(screen.getByText(/another repo/)).toBeInTheDocument();
    expect(screen.queryByText(/unresolved/)).toBeNull();
    fireEvent.click(screen.getByText(/kb:\/\/7b4887ce51d9/));
    expect(onRefClick).not.toHaveBeenCalled();
  });

  it('a broken ref is marked unresolved and is not clickable', () => {
    const onRefClick = vi.fn();
    const fact: Fact = { ...baseFact, refs: [{ raw: 'kb/gone.md', kind: 'broken', path: 'kb/gone.md' }] };
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    expect(screen.getByText(/unresolved/)).toBeInTheDocument();
    fireEvent.click(screen.getByText(/kb\/gone\.md/));
    expect(onRefClick).not.toHaveBeenCalled();
  });

  // A canonical self-qualified ref hops by the REPO-RELATIVE path the SERVER
  // supplies as `path`. The client must not recover it from `raw` — that regex
  // was a second implementation of the kb:// rule, in the one language the
  // ref-classification guard test cannot see.
  it('a canonical kb://<own-id>/ ref hops by the path the server sent', () => {
    const onRefClick = vi.fn();
    const fact: Fact = {
      ...baseFact,
      refs: [{ raw: 'kb://3ec012f5b4d2/kb/x.md', kind: 'fact', path: 'kb/x.md' }],
    };
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    fireEvent.click(screen.getByText(/kb\/x\.md/));
    expect(onRefClick).toHaveBeenCalledWith('kb/x.md');
  });
});
