// The motif's visual identity, such as it is: a mark and no colour.
//
// Every hue in this app names a SUBJECT — domain green, entity blue, a colour
// per fact type. A motif is not a subject; it cuts across them. So it gets the
// one thing none of the others have (a relation glyph, `≈`) and none of the
// thing they all have. The tests below exist because "no colour" is one
// keystroke away from "no entry, silently falls through to path grey".

import { describe, it, expect } from 'vitest';
import { chipStyle, chipColors, MOTIF_GLYPH } from './utils';

describe('the motif chip', () => {
  it('has a palette entry of its own, not the path fallback', () => {
    // chipStyle ends `chipColors[category] || chipColors.path`, so a missing
    // entry does not throw or render blank — it renders as a path chip, which
    // is a different filter category wearing the wrong clothes. Asserting
    // difference from path is the only way that failure shows up.
    const motif = chipStyle('motif', 'failure-presents-as-success');
    expect(motif.bg).not.toBe(chipColors.path.bg);
    expect(motif.text).not.toBe(chipColors.path.text);
  });

  it('carries the ≈ mark', () => {
    expect(chipStyle('motif', 'absence-encodes-value').glyph).toBe(MOTIF_GLYPH);
    expect(MOTIF_GLYPH).toBe('≈');
  });

  it('takes no hue any other category owns', () => {
    // Not an aesthetic assertion: sharing a hue would make the motif read as
    // whichever category already owns it. Text colour is the one a reader
    // actually sees on a chip.
    const motif = chipStyle('motif', 'm').text;
    for (const other of ['domain', 'entity', 'kind', 'origin', 'ep', 'path'] as const) {
      expect(motif).not.toBe(chipColors[other].text);
    }
  });

  it('is the same for every motif — the value carries no colour of its own', () => {
    // Unlike `type`, where each value owns a hue from typeStyles. A motif is
    // hueless as a category, so two motifs must not drift apart.
    const a = chipStyle('motif', 'bypass-defeats-guarantee');
    const b = chipStyle('motif', 'handle-outlives-target');
    expect(a).toEqual(b);
  });
});
