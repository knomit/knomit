import { useEffect, useRef, useState } from 'react';
import type { Dispatch } from 'react';
import type { AppState, Action, FilterChip } from './state';
import { api, parseFilterQuery } from './api';
import { chipColors } from './utils';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

const FACT_CATEGORIES: { key: FilterChip['category']; label: string }[] = [
  { key: 'domain', label: 'Domain' },
  { key: 'entity', label: 'Entity' },
  { key: 'type',   label: 'Type' },
  { key: 'path',   label: 'Path' },
];

const HISTORY_CATEGORIES: { key: FilterChip['category']; label: string }[] = [
  { key: 'ep',   label: 'Episode' },
  { key: 'path', label: 'Path' },
];

// Match a trailing prefix token at end of input
const FACT_PREFIX_RE = /(?:^|\s)(domain|entity|type|path):(\S*)$/;
const HISTORY_PREFIX_RE = /(?:^|\s)(ep|path):(\S*)$/;

export function FilterBar({ state, dispatch }: Props) {
  const isHistory = state.view === 'history';
  const CATEGORIES = isHistory ? HISTORY_CATEGORIES : FACT_CATEGORIES;
  const PREFIX_RE = isHistory ? HISTORY_PREFIX_RE : FACT_PREFIX_RE;

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
          const res = await api.completions(state.repo, category, prefix);
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

  function commitSuggestion(value: string) {
    window.clearTimeout(debounceRef.current);
    const match = PREFIX_RE.exec(inputValue);
    if (!match) return;
    const category = match[1] as FilterChip['category'];
    dispatch({ type: 'ADD_FILTER', chip: { category, value } });
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
      // Path navigation: ArrowRight drills deeper, ArrowLeft goes up
      const prefixMatch = PREFIX_RE.exec(inputValue);
      if (prefixMatch && prefixMatch[1] === 'path') {
        if (e.key === 'ArrowRight') {
          e.preventDefault();
          // Replace the current prefix value with the selected suggestion + '/'
          const selected = suggestions[suggestIdx];
          const before = inputValue.slice(0, prefixMatch.index + (prefixMatch.index > 0 ? 1 : 0));
          setInputValue(before + 'path:' + selected + '/');
          return;
        }
        if (e.key === 'ArrowLeft') {
          e.preventDefault();
          // Go up: remove last path segment from the typed prefix
          const currentPrefix = prefixMatch[2]; // e.g. "kb/tech/go"
          const parts = currentPrefix.split('/');
          if (parts.length > 1) {
            const parent = parts.slice(0, -1).join('/');
            const before = inputValue.slice(0, prefixMatch.index + (prefixMatch.index > 0 ? 1 : 0));
            setInputValue(before + 'path:' + parent);
          }
          return;
        }
      }
    }

    if (e.key === 'Enter' || e.key === ' ') {
      const { chips, text } = parseFilterQuery(inputValue);
      if (chips.length > 0) {
        e.preventDefault();
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
      const res = await api.completions(state.repo, cat, prefix);
      setCategoryValues(res.values || []);
    } catch {
      setCategoryValues([]);
    }
  }

  function drillIntoPath(dir: string) {
    setPathPrefix(dir);
    setCategorySearch('');
    api.completions(state.repo, 'path', dir + '/').then(res => {
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
    dispatch({ type: 'ADD_FILTER', chip: { category: cat, value } });
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

  const hasPrefixMatch = PREFIX_RE.test(inputValue);

  return (
    <div style={{
      display: 'flex',
      alignItems: 'center',
      gap: 6,
      padding: '4px 12px',
      background: '#1e1e1e',
      borderBottom: '1px solid #333',
      flexWrap: 'wrap',
      minHeight: 32,
      position: 'relative',
    }}>
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

        {/* Category picker dropdown */}
        {showCategoryPicker && (
          <div style={{
            position: 'absolute',
            top: '100%',
            left: 0,
            zIndex: 100,
            background: '#252525',
            border: '1px solid #444',
            borderRadius: 4,
            minWidth: 140,
            boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
            display: 'flex',
          }}>
            {/* Category list */}
            <div style={{ borderRight: activeCategory ? '1px solid #333' : 'none' }}>
              {CATEGORIES.map(({ key, label }) => {
                const colors = chipColors[key] || chipColors.path;
                return (
                  <div
                    key={key}
                    onMouseEnter={() => openCategory(key)}
                    onClick={() => openCategory(key)}
                    style={{
                      padding: '6px 12px',
                      fontSize: 12,
                      color: activeCategory === key ? colors.text : '#bbb',
                      background: activeCategory === key ? '#2e2e2e' : 'transparent',
                      cursor: 'pointer',
                      whiteSpace: 'nowrap',
                    }}
                  >
                    {label}
                  </div>
                );
              })}
            </div>

            {/* Values sub-list */}
            {activeCategory && (
              <div style={{ maxHeight: 240, overflowY: 'auto', display: 'flex', flexDirection: 'column' }}>
                <input
                  autoFocus
                  placeholder={`Search ${activeCategory}...`}
                  value={categorySearch}
                  onChange={e => {
                    setCategorySearch(e.target.value);
                    const prefix = activeCategory === 'path' && pathPrefix
                      ? pathPrefix + '/' + e.target.value
                      : e.target.value;
                    const seq = ++catSearchSeqRef.current;
                    api.completions(state.repo, activeCategory, prefix).then(res => {
                      if (seq === catSearchSeqRef.current) setCategoryValues(res.values || []);
                    }).catch(() => {});
                  }}
                  onKeyDown={e => e.stopPropagation()}
                  style={{
                    background: '#1a1a1a',
                    border: 'none',
                    borderBottom: '1px solid #333',
                    outline: 'none',
                    color: '#eee',
                    fontSize: 12,
                    padding: '6px 12px',
                    width: '100%',
                    boxSizing: 'border-box',
                  }}
                />
                {categoryValues.length === 0 && (
                  <div style={{ padding: '6px 12px', fontSize: 11, color: '#555' }}>
                    {categorySearch ? 'No matches' : 'loading...'}
                  </div>
                )}
                {/* Back button when drilled into a path */}
                {activeCategory === 'path' && pathPrefix && (
                  <div
                    onMouseDown={e => { e.preventDefault(); drillUpPath(); }}
                    style={{
                      padding: '4px 12px', fontSize: 12, color: '#888', cursor: 'pointer',
                      whiteSpace: 'nowrap', borderBottom: '1px solid #333', display: 'flex', alignItems: 'center', gap: 4,
                    }}
                    onMouseEnter={e => { (e.currentTarget as HTMLDivElement).style.background = '#333'; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLDivElement).style.background = 'transparent'; }}
                  >
                    ← {pathPrefix}
                  </div>
                )}
                {categoryValues.map(v => {
                  const isPath = activeCategory === 'path';
                  const displayName = isPath ? v.split('/').pop() || v : v;
                  return (
                    <div
                      key={v}
                      style={{
                        padding: '4px 12px', fontSize: 12, color: '#ccc', cursor: 'pointer',
                        whiteSpace: 'nowrap', display: 'flex', alignItems: 'center',
                      }}
                      onMouseEnter={e => { (e.currentTarget as HTMLDivElement).style.background = '#333'; }}
                      onMouseLeave={e => { (e.currentTarget as HTMLDivElement).style.background = 'transparent'; }}
                    >
                      <span
                        onMouseDown={e => { e.preventDefault(); pickCategoryValue(activeCategory, v); }}
                        style={{ flex: 1 }}
                      >{displayName}</span>
                      {isPath && (
                        <span
                          onMouseDown={e => { e.preventDefault(); e.stopPropagation(); drillIntoPath(v); }}
                          style={{ color: '#666', padding: '0 4px', fontSize: 14, cursor: 'pointer' }}
                          title="Browse deeper"
                        >›</span>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Chips */}
      {state.filters.map((chip, i) => {
        const colors = chipColors[chip.category] || chipColors.path;
        return (
          <span key={i} style={{
            background: colors.bg,
            color: colors.text,
            padding: '2px 8px',
            borderRadius: 3,
            fontSize: 11,
            display: 'inline-flex',
            alignItems: 'center',
            gap: 4,
            userSelect: 'none',
          }}>
            {chip.category}:{chip.value}
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
            ? (isHistory ? 'Search commits... (ep:learn or free text)' : 'Filter... (domain:x entity:y or free text)')
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

        {/* Autocomplete dropdown */}
        {hasPrefixMatch && suggestions.length > 0 && (() => {
          const pm = PREFIX_RE.exec(inputValue);
          const typedPrefix = pm ? pm[2] : '';
          const category = pm ? pm[1] : '';
          return (
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
            {/* Header hint */}
            <div style={{ padding: '4px 10px', fontSize: 10, color: '#666', borderBottom: '1px solid #333' }}>
              {category}{typedPrefix ? `: "${typedPrefix}"` : ''}{category === 'path' ? ' — ←/→ to navigate' : ' — type to filter'}
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
        ); })()}
      </div>


    </div>
  );
}
