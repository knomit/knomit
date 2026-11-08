import { useEffect, useRef, useState } from 'react';
import type { CSSProperties } from 'react';

/** The name a rename would give: whitespace collapsed, as the server does. */
function normalTitle(text: string): string {
  return text.trim().split(/\s+/).filter(Boolean).join(' ');
}

/**
 * ChatTitleEdit is the inline rename of a chat's name, used by the rail row and
 * the top bar alike: it opens on a double-click of the name, holding the name
 * selected. Enter or blur saves; Esc cancels; an empty name reverts to the one
 * there was. onDone is called exactly once, with the new name, or with null when
 * nothing is to be saved (cancelled, emptied or unchanged).
 */
export function ChatTitleEdit({ initial, onDone, testId, style }: {
  initial: string;
  onDone: (title: string | null) => void;
  testId: string;
  style?: CSSProperties;
}) {
  const [value, setValue] = useState(initial);
  const ref = useRef<HTMLInputElement>(null);
  const done = useRef(false);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    el.select();
  }, []);
  const finish = (save: boolean) => {
    if (done.current) return;
    done.current = true;
    const t = normalTitle(value);
    onDone(save && t !== '' && t !== normalTitle(initial) ? t : null);
  };
  return (
    <input
      ref={ref}
      type="text"
      data-testid={testId}
      data-nodrag
      value={value}
      aria-label={initial}
      spellCheck={false}
      autoComplete="off"
      onChange={e => setValue(e.target.value)}
      onKeyDown={e => {
        // The key is the edit's: neither the rail's nor the chat's Esc.
        e.stopPropagation();
        if (e.key === 'Enter') { e.preventDefault(); finish(true); }
        else if (e.key === 'Escape') { e.preventDefault(); finish(false); }
      }}
      onBlur={() => finish(true)}
      onClick={e => e.stopPropagation()}
      onMouseDown={e => e.stopPropagation()}
      style={{ ...editBox, ...style }}
    />
  );
}

const editBox: CSSProperties = {
  minWidth: 0, flex: '1 1 auto', font: 'inherit', color: 'inherit', background: '#161616',
  border: '1px solid #3a4150', borderRadius: 4, padding: '1px 5px', outline: 'none', boxSizing: 'border-box',
};
