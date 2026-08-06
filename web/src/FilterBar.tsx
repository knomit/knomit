import { memo, useEffect, useRef, useState } from 'react';
import type { Dispatch } from 'react';
import type { AppState, Action, FilterChip } from './state';
import { isLive, isLensContext, selectTrail } from './state';
import { api, parseFilterQuery } from './api';
import { chipColors, chipStyle, originGlyphs, typeStyles, repoHue, repoHueBg, repoHueBorder } from './utils';
import { TrailBreadcrumb } from './TrailBreadcrumb';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  onJumpTrail?: (index: number) => void;
}

const FACT_CATEGORIES: { key: FilterChip['category']; label: string }[] = [
  { key: 'domain', label: 'Domain' },
  { key: 'entity', label: 'Entity' },
  { key: 'kind',   label: 'Kind' },
  { key: 'type',   label: 'Type' },
  { key: 'origin', label: 'Origin' },
  { key: 'path',   label: 'Path' },
];

// The lens-only source facet, appended to the picker only in lens context.
const REPO_CATEGORY: { key: FilterChip['category']; label: string } = { key: 'repo', label: 'Repo' };

// Above this many values the picker offers a search field. Kind has two and
// Origin three — a search box over them is furniture, not help.
const SEARCHABLE_FROM = 8;

// The facet's own hue, for its rail entry and its column heading. Domain green
// and entity blue are the hues those two carry in the summary panel and on the
// fact body. `type` deliberately has NO hue of its own: every type owns one in
// typeStyles and each ROW wears it, exactly as the summary's Types column does.
function facetHue(cat: FilterChip['category']): string {
  if (cat === 'domain') return 'rgba(119,204,153,0.85)';
  if (cat === 'entity') return 'rgba(136,170,255,0.85)';
  if (cat === 'path')   return '#9aa5b5';
  return '#8a93a3';
}

// A value's mark: the glyph it already wears elsewhere in the app, or a dot in
// the facet's hue when it wears none. Never both — the glyph is drawn in the
// value's own colour, so a dot beside it repeats the only thing it says.
//
// Kind is the one category with no mark anywhere: the fact body gives
// `pragmatic` a text badge and `epistemic` nothing at all.
function valueMark(cat: FilterChip['category'], value: string): { glyph?: string; color: string } {
  if (cat === 'type') {
    const ts = typeStyles[value];
    if (ts) return { glyph: ts.icon, color: ts.color };
  }
  if (cat === 'origin' && originGlyphs[value]) {
    return { glyph: originGlyphs[value], color: chipColors.origin.text };
  }
  if (cat === 'repo')   return { color: repoHue(value) };
  if (cat === 'domain') return { color: 'rgba(119,204,153,0.6)' };
  if (cat === 'entity') return { color: 'rgba(136,170,255,0.6)' };
  if (cat === 'path')   return { color: '#7f8b9c' };
  return { color: (chipColors[cat] || chipColors.path).text };
}

// Categories whose values arrive with NO order worth keeping, so the picker
// imposes one. `completions` runs SELECT DISTINCT … LIMIT with no ORDER BY, and
// the path arm iterates a Go map — its order differs between two calls for the
// same folder.
//
// The others are left exactly as the server sent them, because their order
// already carries meaning: types arrive grouped epistemic-then-pragmatic, kind
// and origin are curated pairs and triples, and repo is in mount order with the
// lens's write repo first. Alphabetising those would destroy information.
const UNORDERED_FACETS = new Set<string>(['domain', 'entity', 'path']);

function orderValues(cat: FilterChip['category'], values: string[]): string[] {
  return UNORDERED_FACETS.has(cat)
    ? [...values].sort((a, b) => a.localeCompare(b))
    : values;
}

// Match a trailing prefix token at end of input. In lens context `repo` joins
// the recognised prefixes so the autocomplete + chip machinery covers it.
const FACT_PREFIX_RE = /(?:^|\s)(domain|entity|type|kind|origin|path):(\S*)$/;
const LENS_PREFIX_RE = /(?:^|\s)(domain|entity|type|kind|origin|path|repo):(\S*)$/;

export const FilterBar = memo(function FilterBar({ state, dispatch, onJumpTrail }: Props) {
  const isLens   = isLensContext(state);
  const lensName = state.context.kind === 'lens' ? state.context.name : '';
  const CATEGORIES = isLens ? [...FACT_CATEGORIES, REPO_CATEGORY] : FACT_CATEGORIES;
  const PREFIX_RE = isLens ? LENS_PREFIX_RE : FACT_PREFIX_RE;

  const [inputValue, setInputValue]               = useState('');
  const [suggestions, setSuggestions]             = useState<string[]>([]);
  const [suggestIdx, setSuggestIdx]               = useState(0);
  const suggestRefs = useRef<(HTMLDivElement | null)[]>([]);
  const [showCategoryPicker, setShowCategoryPicker] = useState(false);
  const [categoryValues, setCategoryValues]       = useState<string[]>([]);
  const [activeCategory, setActiveCategory]       = useState<FilterChip['category'] | null>(null);
  const [categorySearch, setCategorySearch]       = useState('');

  const inputRef        = useRef<HTMLInputElement>(null);
  const debounceRef     = useRef<number>(0);
  const catSearchSeqRef = useRef(0);

  // Track whether the input is focused — used to distinguish user clearing vs onBlur clearing
  const focusedRef = useRef(false);

  // fetchCompletions: in a lens context EVERY category goes to the lens
  // completions endpoint — it unions values across all mounts (and serves the
  // lens-only `repo` category). Routing only `repo` there and the rest to the
  // repo endpoint would suggest write-repo values only, silently hiding
  // read-mount domains/entities/paths. Repo context is unchanged.
  const fetchCompletions = (category: FilterChip['category'] | string, prefix: string): Promise<{ values: string[] }> =>
    isLens
      ? api.lensCompletions(lensName, String(category), prefix)
      : api.completions(state.repo, state.branch, category, prefix);

  // Scroll selected suggestion into view
  useEffect(() => {
    suggestRefs.current[suggestIdx]?.scrollIntoView({ block: 'nearest' });
  }, [suggestIdx]);

  // Sync free-text debounce whenever inputValue changes without a recognised prefix
  useEffect(() => {
    const match = PREFIX_RE.exec(inputValue);
    if (match) {
      // Autocomplete: fetch completions for the recognised prefix
      const category = match[1] as FilterChip['category'];
      const prefix   = match[2];
      window.clearTimeout(debounceRef.current);
      debounceRef.current = window.setTimeout(async () => {
        try {
          const res = await fetchCompletions(category, prefix);
          setSuggestions(res.values || []);
          setSuggestIdx(0);
        } catch {
          setSuggestions([]);
        }
      }, 150);
    } else {
      setSuggestions([]);
      // Only dispatch free-text changes when the input is focused (user is typing).
      // When unfocused, inputValue='' is from onBlur clearing it to show the pill —
      // don't overwrite freeText with empty string.
      if (focusedRef.current) {
        window.clearTimeout(debounceRef.current);
        debounceRef.current = window.setTimeout(() => {
          dispatch({ type: 'SET_FREE_TEXT', text: inputValue });
        }, 300);
      }
    }
    return () => window.clearTimeout(debounceRef.current);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inputValue, state.repo, state.view]);

  // Choosing a `path:` value is a MOVE, not a refinement, so it dispatches the
  // same NAVIGATE the Library rows and the header's ancestors dispatch. The
  // picker survived the location moving to the header — it is a genuinely good
  // way to pick a path — but it must drive the one navigation action, or the
  // back stack and the header would disagree with it about where you are.
  function navOrFilter(category: FilterChip['category'], value: string): Action {
    return category === 'path'
      ? { type: 'NAVIGATE', path: value }
      : { type: 'ADD_FILTER', chip: { category, value } };
  }

  function commitSuggestion(value: string) {
    window.clearTimeout(debounceRef.current);
    const match = PREFIX_RE.exec(inputValue);
    if (!match) return;
    const category = match[1] as FilterChip['category'];
    dispatch(navOrFilter(category, value));
    // Strip the matched prefix token from the input
    const before = inputValue.slice(0, match.index + (match.index > 0 ? 1 : 0));
    setInputValue(before.trimEnd());
    setSuggestions([]);
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if (suggestions.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setSuggestIdx(i => Math.min(i + 1, suggestions.length - 1));
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        setSuggestIdx(i => Math.max(i - 1, 0));
        return;
      }
      if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault();
        commitSuggestion(suggestions[suggestIdx]);
        return;
      }
      if (e.key === 'Escape') {
        setSuggestions([]);
        return;
      }
    }

    if (e.key === 'Enter' || e.key === ' ') {
      const { chips, text, asOf, warnings } = parseFilterQuery(inputValue, () => state.headCommit, { allowRepo: isLens });
      if (warnings.length) dispatch({ type: 'SET_NOTICE', text: warnings.join(' · ') });
      if (asOf || chips.length > 0) {
        e.preventDefault();
        // Time anchors are per-fact in a lens (openFactSource): without an open
        // fact there's no mount to anchor against, so dropping into history would
        // strand the left panel on nothing. Warn and drop the anchor; any chips
        // still apply. Repo context is unaffected — asOf always dispatches there.
        if (asOf && isLens && !state.factPath) {
          dispatch({ type: 'SET_NOTICE', text: 'Open a fact first — time anchors are per-fact in a lens.' });
        } else if (asOf) {
          dispatch({ type: 'SET_AS_OF', asOf });
        }
        chips.forEach(chip => dispatch({ type: 'ADD_FILTER', chip }));
        setInputValue(text);
      } else if (e.key === 'Enter' && inputValue.trim()) {
        e.preventDefault();
        dispatch({ type: 'SET_FREE_TEXT', text: inputValue.trim() });
        // Blur first (sets focusedRef=false) so the debounce won't overwrite with ''
        focusedRef.current = false;
        window.clearTimeout(debounceRef.current);
        setInputValue('');
        inputRef.current?.blur();
      }
      return;
    }

    if (e.key === 'Backspace' && inputValue === '') {
      if (state.filters.length > 0) {
        dispatch({ type: 'REMOVE_FILTER', index: state.filters.length - 1 });
      }
      return;
    }

    if (e.key === 'Escape') {
      setInputValue('');
      inputRef.current?.blur();
      return;
    }
  }

  const [pathPrefix, setPathPrefix] = useState('');

  async function openCategory(cat: FilterChip['category'], prefix = '') {
    setActiveCategory(cat);
    setCategoryValues([]);
    setCategorySearch('');
    if (cat === 'path') setPathPrefix(prefix);
    try {
      const res = await fetchCompletions(cat, prefix);
      setCategoryValues(res.values || []);
    } catch {
      setCategoryValues([]);
    }
  }

  function drillIntoPath(dir: string) {
    setPathPrefix(dir);
    setCategorySearch('');
    fetchCompletions('path', dir + '/').then(res => {
      setCategoryValues(res.values || []);
    }).catch(() => setCategoryValues([]));
  }

  function drillUpPath() {
    const parts = pathPrefix.split('/');
    const parent = parts.length <= 1 ? '' : parts.slice(0, -1).join('/');
    if (parent) {
      drillIntoPath(parent);
    } else {
      openCategory('path', '');
    }
  }

  function pickCategoryValue(cat: FilterChip['category'], value: string) {
    dispatch(navOrFilter(cat, value));
    setShowCategoryPicker(false);
    setActiveCategory(null);
    setCategoryValues([]);
    setCategorySearch('');
    inputRef.current?.focus();
  }

  function closePicker() {
    setShowCategoryPicker(false);
    setActiveCategory(null);
    setCategoryValues([]);
    setCategorySearch('');
    setPathPrefix('');
  }

  // Close picker on outside click or ESC
  const pickerRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!showCategoryPicker) return;
    function onDown(e: MouseEvent) {
      if (pickerRef.current && !pickerRef.current.contains(e.target as Node)) {
        closePicker();
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.stopPropagation();
        closePicker();
      }
    }
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => {
      document.removeEventListener('mousedown', onDown);
      document.removeEventListener('keydown', onKey, true);
    };
  }, [showCategoryPicker]);

  const prefixMatch  = PREFIX_RE.exec(inputValue);
  const hasPrefixMatch = prefixMatch !== null;
  const typedPrefix  = prefixMatch ? prefixMatch[2] : '';
  const prefixCategory = prefixMatch ? prefixMatch[1] : '';

  if (!isLive(state)) {
    return (
      <TrailBreadcrumb
        repo={state.repo}
        branch={state.branch}
        lensName={state.context.kind === 'lens' ? state.context.name : undefined}
        trail={selectTrail(state)}
        titles={state.factTitles}
        onJump={onJumpTrail!}
      />
    );
  }

  return (
    <div style={{
      display: 'flex',
      alignItems: 'center',
      gap: 0,
      padding: '5px 12px',
      background: '#0f0f0f',
      borderBottom: '1px solid #1a1a1a',
      flexWrap: 'wrap',
      minHeight: 34,
      position: 'relative',
    }}>
      {/* Search box wrapper */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: 6,
        flex: 1,
        background: '#161616',
        border: '1px solid #242424',
        borderRadius: 5,
        padding: '0 10px',
        minHeight: 26,
        flexWrap: 'wrap',
      }}>
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="#3a3a3a" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0 }}>
        <circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>
      </svg>

      {/* "+" button — leftmost */}
      <div ref={pickerRef} style={{ position: 'relative' }}>
        <span
          onClick={() => {
            setShowCategoryPicker(v => !v);
            setActiveCategory(null);
            setCategoryValues([]);
          }}
          style={{
            cursor: 'pointer',
            color: '#888',
            fontSize: 16,
            lineHeight: '1',
            padding: '0 4px',
            borderRadius: 3,
            userSelect: 'none',
          }}
          title="Add filter"
        >+</span>

        {/* Category picker. A rail of facets, then that facet's values in
            columns — the summary panel's facet browser, minus the counts it
            gets from the stats histogram (the picker has only /completions,
            which returns bare strings, so there is no rank to show). */}
        {showCategoryPicker && (
          <div data-testid="filter-picker" style={{
            position: 'absolute', top: '100%', left: 0, marginTop: 4, zIndex: 100,
            display: 'flex', width: 600, maxWidth: '80vw',
            background: '#101017', border: '1px solid #262c35', borderRadius: 7,
            boxShadow: '0 12px 34px rgba(0,0,0,0.55)', overflow: 'hidden',
          }}>
            {/* Facet rail */}
            <div style={{ width: 132, flex: 'none', borderRight: '1px solid #1c2029', padding: '9px 0' }}>
              {CATEGORIES.map(({ key, label }) => {
                const on = activeCategory === key;
                const hue = facetHue(key);
                return (
                  <div
                    key={key}
                    data-testid={`picker-cat-${key}`}
                    onMouseEnter={() => openCategory(key)}
                    onClick={() => openCategory(key)}
                    style={{
                      padding: '4px 13px', fontSize: 11.5, cursor: 'pointer', whiteSpace: 'nowrap',
                      color: on ? hue : '#8b95a6',
                      background: on ? '#141a20' : 'transparent',
                      boxShadow: on ? `inset 2px 0 0 ${hue}` : 'none',
                    }}
                  >{label}</div>
                );
              })}
            </div>

            {/* Values */}
            {activeCategory && (() => {
              const values = orderValues(activeCategory, categoryValues);
              // A search box over Kind's two rows is furniture. It appears for a
              // set too big to read whole — or the moment one is typed, so the
              // field can never vanish under the very query that emptied the list.
              const showSearch = values.length > SEARCHABLE_FROM || categorySearch !== '';
              return (
                <div style={{ flex: 1, minWidth: 0, padding: '11px 15px 13px' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 9, minHeight: 22 }}>
                    <div style={{
                      fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5,
                      color: facetHue(activeCategory),
                    }}>{activeCategory}</div>
                    {showSearch && (
                      <input
                        data-testid="picker-search"
                        autoFocus
                        placeholder={`Search ${activeCategory}…`}
                        aria-label={`Search ${activeCategory}`}
                        value={categorySearch}
                        onChange={e => {
                          setCategorySearch(e.target.value);
                          const prefix = activeCategory === 'path' && pathPrefix
                            ? pathPrefix + '/' + e.target.value
                            : e.target.value;
                          const seq = ++catSearchSeqRef.current;
                          fetchCompletions(activeCategory, prefix).then(res => {
                            if (seq === catSearchSeqRef.current) setCategoryValues(res.values || []);
                          }).catch(() => {});
                        }}
                        onKeyDown={e => e.stopPropagation()}
                        style={{
                          marginLeft: 'auto', width: 150, padding: '3px 9px', font: 'inherit', fontSize: 11,
                          background: '#14171f', border: '1px solid #262c35', borderRadius: 4,
                          color: '#cfd6e2', outline: 'none',
                        }}
                      />
                    )}
                  </div>

                  {activeCategory === 'path' && pathPrefix && (
                    <div
                      data-testid="picker-up"
                      onMouseDown={e => { e.preventDefault(); drillUpPath(); }}
                      style={{
                        display: 'flex', alignItems: 'center', gap: 7, fontSize: 11,
                        color: '#7a8593', marginBottom: 7, cursor: 'pointer',
                      }}
                    >{'←'} <span style={{ fontFamily: 'var(--k-font-mono)', color: '#8b95a6' }}>{pathPrefix}</span></div>
                  )}

                  {/* FIXED height, not max-height: the panel must not resize as a
                      search narrows the list or the category changes, or the row
                      under the cursor moves out from under it mid-reach. */}
                  <div data-testid="picker-values" style={{
                    display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))',
                    gap: '0 18px', height: 150, overflowY: 'auto', alignContent: 'start',
                  }}>
                    {values.length === 0 && (
                      <div style={{ fontSize: 11, color: '#5a6675', padding: '4px 0' }}>
                        {categorySearch ? 'No matches' : 'loading…'}
                      </div>
                    )}
                    {values.map(v => {
                      const mark = valueMark(activeCategory, v);
                      const isPath = activeCategory === 'path';
                      return (
                        <div
                          key={v}
                          onMouseDown={e => { e.preventDefault(); pickCategoryValue(activeCategory, v); }}
                          style={{
                            display: 'flex', alignItems: 'center', gap: 6, padding: '2px 0',
                            cursor: 'pointer', minWidth: 0,
                          }}
                        >
                          {mark.glyph
                            ? <span aria-hidden style={{
                                flex: 'none', width: 13, textAlign: 'center', fontSize: 10.5,
                                lineHeight: 1, color: mark.color,
                              }}>{mark.glyph}</span>
                            : <span aria-hidden style={{
                                flex: 'none', width: 4, height: 4, borderRadius: '50%',
                                background: mark.color,
                              }} />}
                          <span
                            data-testid="picker-value"
                            data-value={v}
                            style={{
                              fontSize: 11.5, color: mark.glyph ? mark.color : '#b9c1cd',
                              overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                            }}
                          >{isPath ? (v.split('/').pop() || v) : v}</span>
                          {isPath && (
                            <span
                              data-testid="picker-drill"
                              onMouseDown={e => { e.preventDefault(); e.stopPropagation(); drillIntoPath(v); }}
                              style={{ marginLeft: 'auto', color: '#5a6675', fontSize: 13, padding: '0 2px', cursor: 'pointer' }}
                              title="Browse deeper"
                            >{'›'}</span>
                          )}
                        </div>
                      );
                    })}
                  </div>
                </div>
              );
            })()}
          </div>
        )}
      </div>

      {/* Chips */}
      {state.filters.map((chip, i) => {
        // The lens-only `repo` facet is coloured by the mount's deterministic
        // hue (matching the source badges), not the static per-category palette.
        const isRepo = chip.category === 'repo';
        // chipStyle carries the per-VALUE looks (a type's own colour + glyph, an
        // origin's provenance glyph); repo stays here because its hue is
        // computed from the name rather than looked up.
        const colors = isRepo
          ? { bg: repoHueBg(chip.value), text: repoHue(chip.value), close: repoHue(chip.value), glyph: undefined }
          : chipStyle(chip.category, chip.value);
        return (
          <span key={i} data-category={chip.category} data-value={chip.value}
            data-testid={isRepo ? 'repo-chip' : 'filter-chip'}
            {...(isRepo ? { 'data-repo': chip.value } : {})}
            style={{
            background: colors.bg,
            color: colors.text,
            padding: '2px 8px',
            borderRadius: 3,
            fontSize: 11,
            display: 'inline-flex',
            alignItems: 'center',
            gap: 4,
            userSelect: 'none',
            ...(isRepo ? { border: `1px solid ${repoHueBorder(chip.value)}` } : {}),
          }}>
            {/* Every chip renders the same way now. A path chip used to expand
                into a clickable segment breadcrumb here — the Library's
                location, displayed in the RIGHT column, on the far side of the
                splitter from the list it scoped. The LibraryHeader owns that. */}
            {colors.glyph && (
              <span aria-hidden data-testid="chip-glyph" style={{ fontSize: 10, lineHeight: 1 }}>{colors.glyph}</span>
            )}
            <>{chip.category}:{chip.value}</>
            <span
              style={{ color: colors.close, cursor: 'pointer', fontWeight: 'bold', lineHeight: '1' }}
              onClick={() => dispatch({ type: 'REMOVE_FILTER', index: i })}
            >x</span>
          </span>
        );
      })}

      {/* Free-text pill */}
      {state.freeText && !inputValue && (
        <span style={{
          background: '#2a2a2a',
          color: '#ddd',
          padding: '2px 8px',
          borderRadius: 3,
          fontSize: 11,
          display: 'inline-flex',
          alignItems: 'center',
          gap: 4,
          userSelect: 'none',
          border: '1px solid #444',
        }}>
          {state.freeText}
          <span
            style={{ color: '#888', cursor: 'pointer', fontWeight: 'bold', lineHeight: '1' }}
            onClick={() => dispatch({ type: 'SET_FREE_TEXT', text: '' })}
          >x</span>
        </span>
      )}

      {/* Inline input */}
      <div style={{ position: 'relative', flex: 1, minWidth: 100 }}>
        <input
          id="filter-input"
          ref={inputRef}
          value={inputValue}
          onChange={e => setInputValue(e.target.value)}
          onFocus={() => {
            focusedRef.current = true;
            // If there's a free-text pill, put it back in the input for editing.
            if (state.freeText && !inputValue) {
              setInputValue(state.freeText);
            }
          }}
          onKeyDown={handleKeyDown}
          onBlur={() => {
            focusedRef.current = false;
            // Commit any uncommitted text as free text and clear input so the pill shows.
            // Cancel pending debounce first — onBlur is the authoritative commit point.
            window.clearTimeout(debounceRef.current);
            const trimmed = inputValue.trim();
            if (trimmed && !PREFIX_RE.test(trimmed)) {
              dispatch({ type: 'SET_FREE_TEXT', text: trimmed });
            }
            setInputValue('');
          }}
          placeholder={state.filters.length === 0 && !state.freeText
            ? 'Filter... (domain:x entity:y or free text)'
            : ''}
          style={{
            width: '100%',
            boxSizing: 'border-box',
            background: 'transparent',
            border: 'none',
            outline: 'none',
            color: '#eee',
            fontSize: 12,
            padding: '2px 0',
          }}
        />

        {hasPrefixMatch && suggestions.length > 0 && (
          <div style={{
            position: 'absolute',
            top: '100%',
            left: 0,
            zIndex: 100,
            background: '#252525',
            border: '1px solid #444',
            borderRadius: 4,
            minWidth: 180,
            maxHeight: 220,
            overflowY: 'auto',
            boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          }}>
            <div style={{ padding: '4px 10px', fontSize: 10, color: '#666', borderBottom: '1px solid #333' }}>
              {prefixCategory}{typedPrefix ? `: "${typedPrefix}"` : ''} — type to filter
            </div>
            {suggestions.map((s, idx) => {
              // Highlight the matching prefix portion
              const matchIdx = typedPrefix ? s.toLowerCase().indexOf(typedPrefix.toLowerCase()) : -1;
              return (
                <div
                  key={s}
                  ref={el => { suggestRefs.current[idx] = el; }}
                  onMouseDown={e => { e.preventDefault(); commitSuggestion(s); }}
                  onMouseEnter={() => setSuggestIdx(idx)}
                  style={{
                    padding: '4px 10px',
                    fontSize: 12,
                    color: idx === suggestIdx ? '#eee' : '#aaa',
                    background: idx === suggestIdx ? '#333' : 'transparent',
                    cursor: 'pointer',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {matchIdx >= 0 && typedPrefix ? (<>
                    {s.slice(0, matchIdx)}
                    <span style={{ color: '#fff', fontWeight: 'bold' }}>{s.slice(matchIdx, matchIdx + typedPrefix.length)}</span>
                    {s.slice(matchIdx + typedPrefix.length)}
                  </>) : s}
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* Search-in-flight indicator — a spinning glyph pinned at the end of the
          editor while a relevance query is outstanding (semantic search can take
          a few seconds). Reserves no space when idle. */}
      {state.searching && (
        <svg
          data-testid="search-spinner"
          className="icon-spin"
          width="13" height="13" viewBox="0 0 24 24" fill="none"
          stroke="#7c9" strokeWidth="2.5" strokeLinecap="round"
          style={{ flexShrink: 0, opacity: 0.9 }}
          role="status" aria-label="Searching"
        >
          <path d="M21 12a9 9 0 1 1-6.219-8.56"/>
        </svg>
      )}

      </div> {/* end search box wrapper */}
    </div>
  );
});
