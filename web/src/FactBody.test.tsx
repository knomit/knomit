import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, act } from '@testing-library/react';
import { FactBody, TagCloud } from './FactBody';
import type { Fact, FactRef } from './api';

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
  it('renders the markdown body, domains and entities', () => {
    // The type chip and the confidence/sources boxes left this component: they
    // describe the fact rather than being its content, and they now share one
    // line in the header (FactMetaLine). What is left here IS the content.
    render(<FactBody fact={baseFact} dispatch={vi.fn()} />);

    expect(screen.queryByTestId('fact-type-badge')).toBeNull();
    expect(screen.queryByTestId('fact-meta')).toBeNull();
    expect(screen.getByText('ai')).toBeInTheDocument();
    expect(screen.getByText('distribution')).toBeInTheDocument();
    expect(screen.getByText('Anthropic')).toBeInTheDocument();
    expect(screen.getByText('world').tagName.toLowerCase()).toBe('strong');
  });

  it('tags filter even on a fact you cannot write to', () => {
    // This test used to assert the opposite, and the opposite was a defect:
    // TagCloud gated tag clicks on the same readOnly flag that disables retract
    // and save. But readOnly means "your writes do not go here" — a read mount
    // in a lens, or a historical version — and neither makes it wrong to ask
    // which other facts share this tag. In a multi-mount lens that left the
    // tags inert on nearly every fact.
    const dispatch = vi.fn();
    render(<FactBody fact={baseFact} dispatch={dispatch} />);

    fireEvent.click(screen.getByText('ai'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'domain', value: 'ai' },
    });
  });

  it('tag clicks dispatch ADD_FILTER', () => {
    const dispatch = vi.fn();
    render(<FactBody fact={baseFact} dispatch={dispatch} />);

    fireEvent.click(screen.getByText('Anthropic'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER',
      chip: { category: 'entity', value: 'Anthropic' },
    });
  });

  it('clicking a local ref invokes onRefClick with the ref path', () => {
    const onRefClick = vi.fn();
    render(<FactBody fact={baseFact} dispatch={vi.fn()} onRefClick={onRefClick} />);

    fireEvent.click(screen.getByText(/kb\/local-ref\.md/));
    expect(onRefClick).toHaveBeenCalledWith('kb/local-ref.md');
  });

  it('without onRefClick, local refs render as inert text and do not throw on click', () => {
    render(<FactBody fact={baseFact} dispatch={vi.fn()} />);

    const localRef = screen.getByText(/kb\/local-ref\.md/);
    expect(localRef).toBeInTheDocument();
    fireEvent.click(localRef);
  });

  it('readOnly=true with onRefClick: local refs are still clickable (hop is read-only)', () => {
    const onRefClick = vi.fn();
    render(<FactBody fact={baseFact} dispatch={vi.fn()} onRefClick={onRefClick} />);

    fireEvent.click(screen.getByText(/kb\/local-ref\.md/));
    expect(onRefClick).toHaveBeenCalledWith('kb/local-ref.md');
  });

  it('external (http) refs always render as anchor tags regardless of onRefClick', () => {
    const onRefClick = vi.fn();
    render(<FactBody fact={baseFact} dispatch={vi.fn()} onRefClick={onRefClick} />);

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
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

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
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

      const links = screen.getByTestId('fact-body').querySelectorAll('a');
      expect(links).toHaveLength(2);
      expect(links[0]).toHaveAttribute('href', 'https://www.anthropic.com/engineering');
      expect(links[1]).toHaveAttribute('href', 'https://modelcontextprotocol.io/specification/versioning');
    });

    // Autolinking means a fact that merely mentions a URL renders a live one.
    // The app has no router, so an in-place navigation is a full unload.
    it('opens external links in a new tab with no window.opener handle', () => {
      const body = 'see https://example.com/x and [a link](https://example.com/y)';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

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
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

      const links = screen.getByTestId('fact-body').querySelectorAll('a');
      expect(links.length).toBeGreaterThan(2);
      for (const a of links) expect(a).not.toHaveAttribute('node');
    });

    // GFM footnote refs and backrefs link within the document; sending those to
    // a new tab would open a blank one.
    it('leaves in-document footnote links in place', () => {
      const body = 'A claim.[^1]\n\n[^1]: the note';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

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
      render(<FactBody fact={{ ...baseFact, body: 'see www.example.com today' }} dispatch={vi.fn()} />);

      expect(screen.getByTestId('fact-body').querySelector('a'))
        .toHaveAttribute('href', 'https://www.example.com');
    });

    it('does not rewrite a scheme the author wrote explicitly', () => {
      render(<FactBody fact={{ ...baseFact, body: 'see http://www.example.com today' }} dispatch={vi.fn()} />);

      expect(screen.getByTestId('fact-body').querySelector('a'))
        .toHaveAttribute('href', 'http://www.example.com');
    });

    // An explicit [text](dest) is not a synthesized link: the author typed the
    // destination out by hand, so the scheme is theirs even when the link text
    // happens to be the bare host.
    it('does not rewrite an explicit link whose text happens to be the www. host', () => {
      render(<FactBody fact={{ ...baseFact, body: '[www.example.com](http://www.example.com)' }} dispatch={vi.fn()} />);

      expect(screen.getByTestId('fact-body').querySelector('a'))
        .toHaveAttribute('href', 'http://www.example.com');
    });

    // Nothing in the app defines `sr-only`; markdown.css does, so the GFM
    // footnote label stays out of the rendered prose.
    it('marks the footnote label sr-only so it does not render as a heading', () => {
      const body = 'A claim.[^1]\n\n[^1]: the note';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

      const label = screen.getByTestId('fact-body').querySelector('#footnote-label');
      expect(label).not.toBeNull();
      expect(label).toHaveClass('sr-only');
      // With the label hidden, the section's own class is the only hook left
      // for the rule that separates the notes from the prose.
      expect(screen.getByTestId('fact-body').querySelector('section.footnotes')).not.toBeNull();
    });

    it('renders strikethrough and task lists', () => {
      const body = '~~retracted~~\n\n- [x] done\n- [ ] pending';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

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
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} />);

      expect(screen.getByTestId('fact-body').querySelectorAll('br')).toHaveLength(0);
    });
  });

  it('marks the body with the prose class so markdown escapes the global CSS reset', () => {
    render(<FactBody fact={baseFact} dispatch={vi.fn()} />);

    expect(screen.getByTestId('fact-body')).toHaveClass('k-prose');
  });

  it('src:// refs render as inert (browser cannot open a src: protocol, not a knomit fact path)', () => {
    const fact: Fact = { ...baseFact, refs: [{ raw: 'src://knomit/internal/store/service.go@cfef409', kind: 'source_code' }] };
    const onRefClick = vi.fn();
    render(<FactBody fact={fact} dispatch={vi.fn()} onRefClick={onRefClick} />);

    const el = screen.getByText(/src:\/\/knomit\/internal\/store\/service\.go/);
    expect(el.tagName.toLowerCase()).toBe('span');
    expect(el).not.toHaveAttribute('href');
    fireEvent.click(el);
    expect(onRefClick).not.toHaveBeenCalled();
  });

  // Ref LABELS. The visible text abbreviates the hashes, and swaps a repo id
  // for a name only when that id belongs to a MOUNTED repo. This is formatting
  // of a ref the server has already classified — it never decides kind,
  // clickability, or resolution, which is why it may live here at all.
  describe('reference labels', () => {
    const REPOS = { '3ec012f5b4d2': 'knomit-kb' };

    it('abbreviates a src ref\'s commit and blob, keeping raw as the title', () => {
      const raw = 'src://7b4887ce51d9/web/src/EdgesRail.tsx@b27972ca2d378a20ebccad77a0f73c3aa6a32570:f08b5ecf677c7b9b7106b62dbd20c24eb1a82200';
      const fact: Fact = { ...baseFact, refs: [{ raw, kind: 'source_code' }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} onRefClick={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText('→ src://7b4887ce51d9/web/src/EdgesRail.tsx@b27972ca…:f08b5ecf…')).toBeInTheDocument();
      // The raw citation is what a reader copies, so it must stay reachable.
      expect(screen.getByTitle(new RegExp(raw.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))).toBeInTheDocument();
    });

    // The tooltip's whole job is to save the reader assembling the command by
    // hand, which a `<blob>` placeholder did not do.
    describe('retrieval hint', () => {
      const titleOf = (raw: string, kind: FactRef['kind'] = 'source_code') => {
        const { container } = render(
          <FactBody fact={{ ...baseFact, refs: [{ raw, kind }] }} dispatch={vi.fn()} repoNames={REPOS} />,
        );
        return container.querySelector('[title]')?.getAttribute('title') ?? '';
      };

      it('carries the real 40-hex blob, not a placeholder', () => {
        const title = titleOf('src://7b4887ce51d9/x.go@b27972ca2d378a20ebccad77a0f73c3aa6a32570:f08b5ecf677c7b9b7106b62dbd20c24eb1a82200');
        expect(title).toContain('git cat-file blob f08b5ecf677c7b9b7106b62dbd20c24eb1a82200');
        expect(title).not.toContain('<blob>');
      });

      // A line range is addressing, not part of the object id — it must not
      // ride along into the hash and break the command.
      it('does not let a line range leak into the blob', () => {
        const title = titleOf('src://7b4887ce51d9/x.go@b27972ca2d378a20ebccad77a0f73c3aa6a32570:f08b5ecf677c7b9b7106b62dbd20c24eb1a82200#L241-L259');
        // Asserted as the END of the title: a trailing "#L241-L259" glued to
        // the hash would still satisfy a bare toContain.
        expect(title.endsWith('git cat-file blob f08b5ecf677c7b9b7106b62dbd20c24eb1a82200')).toBe(true);
      });

      // No blob to cat: offer the weaker command that does exist rather than an
      // unrunnable one, and say why it is weaker.
      it('falls back to git show for a legacy ref with no blob', () => {
        const title = titleOf('src://knomit/internal/legacy.go@ca1c272');
        expect(title).toContain('git show ca1c272:internal/legacy.go');
        expect(title).not.toContain('cat-file');
        expect(title).toContain('did not exist at that commit');
      });

      it('offers no command when there is no version at all', () => {
        const title = titleOf('src://knomit/internal/legacy.go');
        expect(title).not.toContain('git ');
      });
    });

    // A native title is browser chrome: readable, not selectable. The button is
    // how a 40-hex hash gets out of the UI without being retyped.
    describe('copy button', () => {
      const renderRef = (raw: string) => render(
        <FactBody fact={{ ...baseFact, refs: [{ raw, kind: 'source_code' }] }}
          dispatch={vi.fn()} onRefClick={vi.fn()} repoNames={REPOS} />,
      );

      it('copies the cat-file command with the real blob', async () => {
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.assign(navigator, { clipboard: { writeText } });
        renderRef('src://7b4887ce51d9/x.go@b27972ca2d378a20ebccad77a0f73c3aa6a32570:f08b5ecf677c7b9b7106b62dbd20c24eb1a82200');

        fireEvent.click(screen.getByTestId('source-ref-copy'));
        expect(writeText).toHaveBeenCalledWith('git cat-file blob f08b5ecf677c7b9b7106b62dbd20c24eb1a82200');
      });

      it('copies the git show fallback for a legacy ref', () => {
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.assign(navigator, { clipboard: { writeText } });
        renderRef('src://knomit/internal/legacy.go@ca1c272');

        fireEvent.click(screen.getByTestId('source-ref-copy'));
        expect(writeText).toHaveBeenCalledWith('git show ca1c272:internal/legacy.go');
      });

      it('offers no button when there is no command to run', () => {
        renderRef('src://knomit/internal/legacy.go');
        expect(screen.queryByTestId('source-ref-copy')).toBeNull();
      });

      // The row must stay inert. The copy button is a separate target; the ref
      // text itself is not a fact path and was once wrongly fed to onRefClick.
      it('does not make the ref clickable', () => {
        const onRefClick = vi.fn();
        const raw = 'src://7b4887ce51d9/x.go@b27972ca2d378a20ebccad77a0f73c3aa6a32570:f08b5ecf677c7b9b7106b62dbd20c24eb1a82200';
        render(<FactBody fact={{ ...baseFact, refs: [{ raw, kind: 'source_code' }] }}
          dispatch={vi.fn()} onRefClick={onRefClick} repoNames={REPOS} />);

        fireEvent.click(screen.getByText(/src:\/\/7b4887ce51d9\/x\.go/));
        expect(onRefClick).not.toHaveBeenCalled();
        expect(screen.getByTestId('source-ref').querySelector('a')).toBeNull();
      });

      // Clipboard rejects in insecure contexts and when permission is denied.
      // That must not surface a "Copied" state that did not happen, and must
      // not throw an unhandled rejection.
      it('survives an unavailable clipboard without claiming success', async () => {
        const writeText = vi.fn().mockRejectedValue(new Error('denied'));
        Object.assign(navigator, { clipboard: { writeText } });
        renderRef('src://7b4887ce51d9/x.go@b27972ca2d378a20ebccad77a0f73c3aa6a32570:f08b5ecf677c7b9b7106b62dbd20c24eb1a82200');

        const btn = screen.getByTestId('source-ref-copy');
        fireEvent.click(btn);
        await act(async () => { await Promise.resolve(); });
        expect(btn).not.toHaveAttribute('title', 'Copied');
      });
    });

    // A src:// id is the SOURCE repo's root commit; repoNames is keyed by
    // KB-STORE ids. They are different namespaces, so this must NOT resolve —
    // and must not mangle the id either.
    it('leaves a src ref\'s repo id alone — it is not a KB-store id', () => {
      const fact: Fact = { ...baseFact, refs: [{
        raw: 'src://7b4887ce51d9/x.go@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        kind: 'source_code',
      }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText('→ src://7b4887ce51d9/x.go@aaaaaaaa…:bbbbbbbb…')).toBeInTheDocument();
    });

    it('drops the "(source)" marker — src:// in the label already says so', () => {
      const fact: Fact = { ...baseFact, refs: [{
        raw: 'src://7b4887ce51d9/x.go@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        kind: 'source_code',
      }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.queryByText(/\(source\)/)).toBeNull();
    });

    // The case the name overlay actually exists for: a kb:// id IS a KB-store
    // id, so a foreign ref into another mounted repo reads as that repo.
    it('names a foreign kb ref by its mounted repo, and marks it another repo', () => {
      const fact: Fact = { ...baseFact, refs: [{ raw: 'kb://3ec012f5b4d2/kb/z.md', kind: 'foreign' }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText('→ kb://knomit-kb/kb/z.md')).toBeInTheDocument();
      // Still marked: naming the repo does not make it ours to open.
      // The marker moved out of the ref's own text into the row's kind column,
      // where every kind now says what it is instead of two of five doing so.
      expect(screen.getByText('another repo')).toBeInTheDocument();
    });

    it('keeps the id when it names no mounted repo', () => {
      const fact: Fact = { ...baseFact, refs: [{ raw: 'kb://ffffffffffff/kb/z.md', kind: 'foreign' }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText('→ kb://ffffffffffff/kb/z.md')).toBeInTheDocument();
    });

    it('preserves a line range', () => {
      const fact: Fact = { ...baseFact, refs: [{
        raw: 'src://7b4887ce51d9/x.go@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb#L241-L259',
        kind: 'source_code',
      }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText('→ src://7b4887ce51d9/x.go@aaaaaaaa…:bbbbbbbb…#L241-L259')).toBeInTheDocument();
    });

    // A legacy src form: named repo, short commit, no blob. Nothing to
    // abbreviate — and a hash already under the cut must not gain a "…" that
    // claims there is more of it.
    it('leaves a legacy src ref untouched', () => {
      const raw = 'src://knomit/internal/legacy.go@ca1c272';
      const fact: Fact = { ...baseFact, refs: [{ raw, kind: 'source_code' }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText(`→ ${raw}`)).toBeInTheDocument();
    });

    // The failure mode that matters: an unparseable ref must render exactly as
    // it always did, never a mangled fragment.
    it('falls back to raw for a shape the label parser does not match', () => {
      const raw = 'src://no-slash-here';
      const fact: Fact = { ...baseFact, refs: [{ raw, kind: 'source_code' }] };
      render(<FactBody fact={fact} dispatch={vi.fn()} repoNames={REPOS} />);

      expect(screen.getByText(`→ ${raw}`)).toBeInTheDocument();
    });
  });

  it('file:/// refs render as inert (not a knomit fact path)', () => {
    const fact: Fact = { ...baseFact, refs: [{ raw: 'file:///etc/hosts', kind: 'url' }] };
    const onRefClick = vi.fn();
    render(<FactBody fact={fact} dispatch={vi.fn()} onRefClick={onRefClick} />);

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
    render(<FactBody fact={fact} dispatch={vi.fn()} onRefClick={onRefClick} />);

    expect(screen.getByText(/another repo/)).toBeInTheDocument();
    expect(screen.queryByText(/unresolved/)).toBeNull();
    fireEvent.click(screen.getByText(/kb:\/\/7b4887ce51d9/));
    expect(onRefClick).not.toHaveBeenCalled();
  });

  it('a broken ref is marked unresolved and is not clickable', () => {
    const onRefClick = vi.fn();
    const fact: Fact = { ...baseFact, refs: [{ raw: 'kb/gone.md', kind: 'broken', path: 'kb/gone.md' }] };
    render(<FactBody fact={fact} dispatch={vi.fn()} onRefClick={onRefClick} />);

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
    render(<FactBody fact={fact} dispatch={vi.fn()} onRefClick={onRefClick} />);

    fireEvent.click(screen.getByText(/kb\/x\.md/));
    expect(onRefClick).toHaveBeenCalledWith('kb/x.md');
  });
});

describe('TagCloud heading', () => {
  it('renders a heading when labelled, with its count', () => {
    // "Types · 1", the summary panel's own heading form — the facet named, then
    // how many values it holds.
    render(<TagCloud label="Types" entries={[['synthesis', 3]]} color="136,170,255" onTagClick={vi.fn()} />);
    expect(screen.getByText(/^Types · 1$/)).toBeInTheDocument();
  });

  it('renders NO heading element for an empty label', () => {
    // An unconditional heading left a blank uppercase row and its 10px margin
    // above the tags — a dead gap that made passing a label load-bearing.
    const { container } = render(
      <TagCloud label="" entries={[['synthesis', 3]]} color="136,170,255" onTagClick={vi.fn()} />);
    // Only the tag row remains under the wrapper, no empty heading sibling.
    expect(container.firstElementChild!.children).toHaveLength(1);
    expect(screen.getByTestId('tag-item')).toBeInTheDocument();
  });
});

describe('FactBody — the metadata block', () => {
  it('flows a facet\'s values across its half instead of one tall column', () => {
    // Four domains stacked in a 545px column left most of that half empty.
    render(<FactBody fact={baseFact} dispatch={vi.fn()} />);
    const values = screen.getAllByTestId('tag-item')[0].parentElement!;
    expect(values.style.display).toBe('grid');
    expect(values.style.gridTemplateColumns).toContain('auto-fill');
  });

  it('rules off the prose before the metadata starts', () => {
    render(<FactBody fact={baseFact} dispatch={vi.fn()} />);
    const block = screen.getByTestId('fact-metadata');
    expect(block.style.borderTop).not.toBe('');
  });

  it('draws no rule for a fact with no metadata at all', () => {
    // An empty bordered block is a line across the panel under nothing.
    render(<FactBody fact={{ ...baseFact, domain: [], entities: [], refs: [] }}
      dispatch={vi.fn()} />);
    expect(screen.queryByTestId('fact-metadata')).toBeNull();
  });

  it('still rules off when only one of the three is present', () => {
    render(<FactBody fact={{ ...baseFact, domain: [], entities: [], refs: [{ raw: 'https://x.test', kind: 'url' }] }}
      dispatch={vi.fn()} />);
    expect(screen.getByTestId('fact-metadata')).toBeTruthy();
  });
});
