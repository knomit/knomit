import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { ChatTurnFooter, cardSide } from './ChatTurnFooter';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

const usage = { input: 100, output: 20, cacheRead: 0, cacheWrite: 0 };

// jsdom has no layout: give the three boxes the test cares about a position.
function layout(rects: Record<string, { top: number; height: number }>) {
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    const r = rects[this.getAttribute('data-testid') ?? ''] ?? { top: 0, height: 0 };
    return { top: r.top, bottom: r.top + r.height, left: 0, right: 0, width: 0, height: r.height, x: 0, y: r.top, toJSON: () => ({}) };
  });
}

function mount() {
  render(
    <div data-testid="scroller" style={{ overflowY: 'auto' }}>
      <ChatTurnFooter usage={usage} costUsd={0.01} answer="a" sessionCostUsd={0.02} sessionTurns={2} />
    </div>,
  );
  fireEvent.mouseEnter(screen.getByTestId('chat-usage'));
  return screen.getByTestId('chat-cost-card');
}

describe('the cost card opens where it fits', () => {
  it('cardSide: above only when the room above holds the card and its gap', () => {
    expect(cardSide(300, 160)).toBe('above');
    expect(cardSide(168, 160)).toBe('above');
    expect(cardSide(167, 160)).toBe('below');
    expect(cardSide(0, 160)).toBe('below');
  });

  // SABOTAGE: make the effect always choose 'above' (or cardSide return 'above') -> red.
  it('opens above when the scroller has room over the footer', () => {
    layout({ scroller: { top: 40, height: 600 }, 'chat-usage': { top: 400, height: 17 }, 'chat-cost-card': { top: 0, height: 160 } });
    const card = mount();
    expect(card.getAttribute('data-side')).toBe('above');
    expect(card.style.bottom).toContain('100%');
  });

  it('opens below when the footer is near the top of the scroller, so no row is cut off', () => {
    layout({ scroller: { top: 40, height: 600 }, 'chat-usage': { top: 90, height: 17 }, 'chat-cost-card': { top: 0, height: 160 } });
    const card = mount();
    expect(card.getAttribute('data-side')).toBe('below');
    expect(card.style.top).toContain('100%');
    expect(card.style.bottom).toBe('auto');
  });
});
