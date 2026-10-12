import { useCallback, useLayoutEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { PencilIcon } from './icons';
import { btn, cardIconBtn } from './manageStyles';

// useDescriptionEditor owns the draft behind a repo's README or a lens's note.
//
// It is a hook rather than state inside DescriptionBody because the two halves
// of this editor are rendered by two different components: the text sits in the
// block's body, its controls sit in the block's heading. Both need one draft,
// and the only place that renders both is the caller. See DescriptionActions
// for why the controls are up there.
export function useDescriptionEditor({ markdown, maxBytes, editing, onEditing, onSave }: {
  markdown: string;
  // Byte cap the server enforces for THIS destination — a repo's README.md and
  // a lens's note share this editor but not their limits.
  maxBytes: number;
  editing: boolean;
  onEditing: (editing: boolean) => void;
  onSave: (md: string) => Promise<void>;
}) {
  // null = untouched, so the editor falls through to `markdown` and the pencil
  // that opens it needs no seeding step. Closing the editor clears it back to
  // null, which is also what makes a Cancel discard the draft.
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const text = draft ?? markdown;
  const cancel = () => { setDraft(null); setErr(''); onEditing(false); };
  const save = async () => {
    setBusy(true); setErr('');
    try { await onSave(text); setDraft(null); setErr(''); onEditing(false); }
    catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  // The cap is a byte count server-side, so measure bytes: a draft of em-dashes
  // and smart quotes hits the limit at a third of its character count. Only
  // shown once the draft is within sight of the cap — a counter on every edit
  // is noise for the 200-byte case, but a lens's 4 KiB is close enough to a
  // page of notes that silence would let the user write past it and lose the
  // Save to a 422.
  const bytes = new TextEncoder().encode(text).length;
  const over = bytes > maxBytes;
  const showCount = bytes > maxBytes * 0.8;

  // Reading and writing are the same box, so the pencil swaps what is in it and
  // resizes nothing. The rendered body is content-sized within its band, so the
  // editor cannot name a height of its own — it would be right for one README
  // and wrong for the next. Instead the read height is measured after every
  // read render and handed to the textarea, which is border-box like the body
  // it replaces, so the outer box is identical.
  //
  // The empty description is the one case with nothing to measure, and nobody
  // writes a README from scratch in the 20px a placeholder line would give.
  //
  // A CALLBACK ref, not a RefObject: the description block attaches this to a
  // <div> (DescriptionBody's markdown render), but the licence block attaches
  // it directly to its <pre> read view — DescriptionBody never renders the
  // licence's read view, so there is no <div> for licEditor to measure. A
  // `RefObject<HTMLElement | null>` reads fine for either call site but is
  // NOT assignable to `Ref<HTMLDivElement>` or `Ref<HTMLPreElement>` — the two
  // element types are unrelated as far as the object-identity ref's exact
  // generic is concerned, so `tsc -b` (unlike `tsc --noEmit -p .`, which
  // missed this) rejects it at both attach sites. A callback ref sidesteps
  // that: `Ref<T>` for any element T also accepts `(el: T | null) => void`,
  // and function parameters check contravariantly, so one callback typed at
  // the wider `HTMLElement | null` is assignable everywhere a narrower
  // `HTMLDivElement | null` or `HTMLPreElement | null` callback is expected.
  const elRef = useRef<HTMLElement | null>(null);
  const bodyRef = useCallback((el: HTMLElement | null) => { elRef.current = el; }, []);
  const readHeight = useRef<number | null>(null);
  useLayoutEffect(() => {
    if (!editing && elRef.current) readHeight.current = elRef.current.offsetHeight;
  });
  const height = markdown ? (readHeight.current ?? DESC_BODY_MAX) : DESC_BODY_BLANK;

  return {
    markdown, maxBytes, editing, text, setDraft, busy, err, save, cancel,
    bytes, over, showCount, bodyRef, height,
    edit: () => onEditing(true),
  };
}
export type DescriptionEditor = ReturnType<typeof useDescriptionEditor>;

// DescriptionActions is the description block's heading control: a pencil while
// reading, Save and Cancel while writing.
//
// Save and Cancel used to sit under the textarea, with a hint line above them.
// That is the conventional place and it was wrong here, because it makes the
// act of clicking the pencil push every block below the description ~55px down
// the page — the reader's eye is on the text, and the text is what moves. The
// heading already reserves a right-aligned slot, and swapping a control for two
// controls inside a slot of fixed height moves nothing at all.
//
// The hint line did not need a new home: it said "Markdown · committed to
// README.md on the agent branch" directly under a heading that already reads
// "README.md, committed to the agent branch". The byte counter did, and it is
// here, where it can appear and disappear without reflowing the page either.
export function DescriptionActions({ editor, label, testIdPrefix = 'repo-description' }: {
  editor: DescriptionEditor; label: string;
  // Lets a second block (the licence) reuse this control without colliding
  // testids with the description block it sits beside on the same page.
  testIdPrefix?: string;
}) {
  const { editing, busy, over, bytes, maxBytes, showCount, save, cancel } = editor;
  if (!editing) {
    return (
      <button type="button" className="k-bare" data-testid={`${testIdPrefix}-edit`}
        title={label} aria-label={label}
        style={cardIconBtn()} onClick={editor.edit}>
        <PencilIcon color="#888" size={13} />
      </button>
    );
  }
  return (
    <>
      {showCount && (
        <span data-testid={`${testIdPrefix}-count`}
          style={{ fontSize: 11, color: over ? '#f88' : '#888', whiteSpace: 'nowrap' }}>
          {bytes.toLocaleString()} / {maxBytes.toLocaleString()} bytes
        </span>
      )}
      <button type="button" data-testid={`${testIdPrefix}-save`} style={descBtn(busy || over, 'primary')} disabled={busy || over}
        title={over ? `too long by ${(bytes - maxBytes).toLocaleString()} bytes` : undefined} onClick={save}>
        {busy ? 'Saving…' : 'Save'}
      </button>
      <button type="button" data-testid={`${testIdPrefix}-cancel`} style={descBtn(busy)} disabled={busy} onClick={cancel}>Cancel</button>
    </>
  );
}

// DescriptionBody renders a repo's or lens's description as markdown, or the
// raw markdown in a textarea while it is being edited. Reading and writing
// share one component because they are one box: same width, same height, same
// position on the page.
//
// It used to live behind a disclosure. In a boxed dialog that was right: the
// pane's whole budget was about two cards deep, so a document had to fold away.
// Now the page is the window and a README is the most-read thing on it, so it
// renders open and the fold is gone. The rendered body still scrolls within a
// bounded band — a long manifest must not push the wiring blocks off the page —
// and the editor is a plain textarea over the raw markdown, with no rich-text
// layer that could rewrite what gets committed.
export function DescriptionBody({ editor, readOnly, containerTestId = 'repo-description', textareaTestId = 'repo-description-input' }: {
  editor: DescriptionEditor; readOnly: boolean;
  // Same reason as DescriptionActions' testIdPrefix: the licence block reuses
  // this component for its EDITOR only (its read view is <pre>, never this),
  // and needs its own textarea identity — `data-testid="license-textarea"` —
  // without colliding with the description block's, which sits on the page
  // at the same time.
  containerTestId?: string; textareaTestId?: string;
}) {
  const { markdown, editing, text, busy, err, setDraft, bodyRef, height } = editor;
  return (
    <div data-testid={containerTestId}>
      {editing ? (
        <>
          <textarea
            data-testid={textareaTestId}
            value={text}
            disabled={busy}
            onChange={e => setDraft(e.target.value)}
            style={{ ...descTextarea, height }}
            spellCheck={false}
          />
          {/* The one thing still allowed to grow the block, because a failed
              save is worth a shove: it says the text you are looking at is not
              what is stored. */}
          {err && <div style={{ fontSize: 12, color: '#f88', marginTop: 6 }}>{err}</div>}
        </>
      ) : markdown ? (
        // Full width: nothing shares the row, so a README that scrolls uses the
        // whole column instead of wrapping early around a 13px glyph.
        <div ref={bodyRef} className="k-prose"
          style={{ minHeight: DESC_BODY_MIN, maxHeight: DESC_BODY_MAX, overflowY: 'auto', color: '#bbb', fontSize: 13, lineHeight: 1.6 }}>
          <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{markdown}</ReactMarkdown>
        </div>
      ) : (
        // "in the heading" because the pencil no longer sits on this row — it
        // moved to the block heading's right edge, too far for a bare "the
        // pencil" to point at anything the eye finds locally.
        <div style={{ fontSize: 13, color: '#666' }}>
          No description yet.{!readOnly && ' Use the pencil in the heading to write one in markdown.'}
        </div>
      )}
    </div>
  );
}

// The description block's height band. MAX caps the rendered scroller — a long
// manifest must not push the wiring blocks off the page — and MIN keeps a
// two-line description from rendering in a box too small to edit in. The editor
// inherits both by measuring the body it replaces, so the band governs reading
// and writing alike and the pencil never resizes anything. BLANK is the one
// height the band does not set: an empty description has no body to measure.
const DESC_BODY_MIN = 120;
const DESC_BODY_MAX = 360;
const DESC_BODY_BLANK = 240;
// descBtn is Save/Cancel sized to sit in a block heading. The height is exactly
// cardIconBtn's, so the heading row is the same height whether it holds a pencil
// or these two — which is the whole point of putting them there.
const descBtn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({
  ...btn(disabled, variant),
  display: 'inline-flex', alignItems: 'center',
  height: 24, padding: '0 10px', fontSize: 12,
});
// descTextarea edits raw markdown, so it is monospaced — a repo's README.md is
// a document, not a caption. Its height is NOT here: DescriptionBody sets it
// from the rendered body's measured height so the pencil never resizes the box.
const descTextarea: React.CSSProperties = {
  // `block`, not the default inline-block: as an inline box it sits on a text
  // baseline and its line box adds ~6px of descender under it, which is exactly
  // 6px of page movement when it replaces the rendered body.
  width: '100%', display: 'block', boxSizing: 'border-box', resize: 'vertical',
  background: '#0c0c0c', border: '1px solid #333', borderRadius: 5, color: '#ddd',
  padding: '9px 11px', fontSize: 12.5, lineHeight: 1.6,
  fontFamily: 'var(--k-font-mono)',
};
