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
    render(<FactBody fact={baseFact} navigate={() => {}} dispatch={vi.fn()} readOnly={false} />);

    expect(screen.getByTestId('fact-type-badge')).toBeInTheDocument();
    expect(screen.getByText('0.87')).toBeInTheDocument();
    expect(screen.getByText('3')).toBeInTheDocument();
    expect(screen.getByText('ai')).toBeInTheDocument();
    expect(screen.getByText('distribution')).toBeInTheDocument();
    expect(screen.getByText('Anthropic')).toBeInTheDocument();
    // Markdown bold renders as <strong>world</strong>.
    expect(screen.getByText('world').tagName.toLowerCase()).toBe('strong');
  });

  it('readOnly=true: tag clicks do not dispatch and local refs are hidden', () => {
    const dispatch = vi.fn();
    const navigate = vi.fn();
    render(<FactBody fact={baseFact} navigate={navigate} dispatch={dispatch} readOnly={true} />);

    fireEvent.click(screen.getByText('ai'));
    expect(dispatch).not.toHaveBeenCalled();

    // External ref is shown.
    expect(screen.getByText(/example\.com\/paper/)).toBeInTheDocument();
    // Local ref is NOT shown in body (will be in outgoing rail).
    expect(screen.queryByText(/kb\/local-ref\.md/)).toBeNull();
  });

  it('readOnly=false: tag clicks dispatch ADD_FILTER', () => {
    const dispatch = vi.fn();
    render(<FactBody fact={baseFact} navigate={() => {}} dispatch={dispatch} readOnly={false} />);

    fireEvent.click(screen.getByText('Anthropic'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER',
      chip: { category: 'entity', value: 'Anthropic' },
    });
  });
});
