/** The space between the footer and its cost card. */
export const CARD_GAP = 8;

/** Which side of the footer the cost card opens on, given the room between the
 *  footer and the top of the scroller it sits in, and the card's own height. */
export function cardSide(roomAbove: number, cardHeight: number): 'above' | 'below' {
  return roomAbove >= cardHeight + CARD_GAP ? 'above' : 'below';
}

