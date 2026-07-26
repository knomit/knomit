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
  refs: ['https://example.com/paper', 'kb/local-ref.md'],
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

    it('renders strikethrough and task lists', () => {
      const body = '~~retracted~~\n\n- [x] done\n- [ ] pending';
      render(<FactBody fact={{ ...baseFact, body }} dispatch={vi.fn()} readOnly={false} />);

      expect(screen.getByText('retracted').tagName.toLowerCase()).toBe('del');
      const boxes = screen.getByTestId('fact-body').querySelectorAll('input[type="checkbox"]');
      expect(boxes).toHaveLength(2);
      expect((boxes[0] as HTMLInputElement).checked).toBe(true);
      expect((boxes[1] as HTMLInputElement).checked).toBe(false);
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
    const fact: Fact = { ...baseFact, refs: ['src://knomit/internal/store/service.go@cfef409'] };
    const onRefClick = vi.fn();
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    const el = screen.getByText(/src:\/\/knomit\/internal\/store\/service\.go/);
    expect(el.tagName.toLowerCase()).toBe('span');
    expect(el).not.toHaveAttribute('href');
    fireEvent.click(el);
    expect(onRefClick).not.toHaveBeenCalled();
  });

  it('file:/// refs render as inert (not a knomit fact path)', () => {
    const fact: Fact = { ...baseFact, refs: ['file:///etc/hosts'] };
    const onRefClick = vi.fn();
    render(<FactBody fact={fact} dispatch={vi.fn()} readOnly={false} onRefClick={onRefClick} />);

    const el = screen.getByText(/file:\/\/\/etc\/hosts/);
    expect(el.tagName.toLowerCase()).toBe('span');
    expect(el).not.toHaveAttribute('href');
    fireEvent.click(el);
    expect(onRefClick).not.toHaveBeenCalled();
  });
});
