import { useEffect, useRef, useState } from 'react';
import type { Dispatch } from 'react';
import type { AppState, Action, FilterChip } from './state';
import { api, parseFilterQuery } from './api';
import { chipColors } from './utils';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

const CATEGORIES: { key: FilterChip['category']; label: string }[] = [
  { key: 'domain', label: 'Domain' },
  { key: 'entity', label: 'Entity' },
  { key: 'type',   label: 'Type' },
  { key: 'path',   label: 'Path' },
];

// Match a trailing prefix token like "domain:go" or "entity:" at end of input
const PREFIX_RE = /(?:^|\s)(domain|entity|type|path):(\S*)$/;

export function FilterBar({ state, dispatch }: Props) {
  const [inputValue, setInputValue]               = useState('');
  const [suggestions, setSuggestions]             = useState<string[]>([]);
  const [suggestIdx, setSuggestIdx]               = useState(0);
  const [showCategoryPicker, setShowCategoryPicker] = useState(false);
  const [categoryValues, setCategoryValues]       = useState<string[]>([]);
  const [activeCategory, setActiveCategory]       = useState<FilterChip['category'] | null>(null);
  const [categorySearch, setCategorySearch]       = useState('');

  const inputRef    = useRef<HTMLInputElement>(null);
  const debounceRef = useRef<number>(0);

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
      // Debounce free-text dispatch
      window.clearTimeout(debounceRef.current);
      debounceRef.current = window.setTimeout(() => {
        dispatch({ type: 'SET_FREE_TEXT', text: inputValue });
      }, 300);
    }
    return () => window.clearTimeout(debounceRef.current);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inputValue, state.repo]);

  function commitSuggestion(value: string) {
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
        setInputValue('');
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

  async function openCategory(cat: FilterChip['category']) {
    setActiveCategory(cat);
    setCategoryValues([]);
    setCategorySearch('');
    try {
      const res = await api.completions(state.repo, cat, '');
      setCategoryValues(res.values || []);
    } catch {
      setCategoryValues([]);
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

  // Close picker on outside click
  const pickerRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!showCategoryPicker) return;
    function onDown(e: MouseEvent) {
      if (pickerRef.current && !pickerRef.current.contains(e.target as Node)) {
        setShowCategoryPicker(false);
        setActiveCategory(null);
        setCategoryValues([]);
      }
    }
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
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
                    api.completions(state.repo, activeCategory, e.target.value).then(res => {
                      setCategoryValues(res.values || []);
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
                {categoryValues.map(v => (
                  <div
                    key={v}
                    onMouseDown={e => { e.preventDefault(); pickCategoryValue(activeCategory, v); }}
                    style={{
                      padding: '4px 12px',
                      fontSize: 12,
                      color: '#ccc',
                      cursor: 'pointer',
                      whiteSpace: 'nowrap',
                    }}
                    onMouseEnter={e => { (e.currentTarget as HTMLDivElement).style.background = '#333'; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLDivElement).style.background = 'transparent'; }}
                  >
                    {v}
                  </div>
                ))}
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
            // If there's a free-text pill, put it back in the input for editing.
            // Keep freeText in state so search results stay visible.
            if (state.freeText && !inputValue) {
              setInputValue(state.freeText);
            }
          }}
          onKeyDown={handleKeyDown}
          onBlur={() => {
            // Commit any uncommitted text as free text and clear input so the pill shows
            const trimmed = inputValue.trim();
            if (trimmed && !PREFIX_RE.test(trimmed)) {
              window.clearTimeout(debounceRef.current);
              dispatch({ type: 'SET_FREE_TEXT', text: trimmed });
              setInputValue('');
            }
          }}
          placeholder={state.filters.length === 0 && !state.freeText ? 'Filter... (domain:x entity:y or free text)' : ''}
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
            maxHeight: 200,
            overflowY: 'auto',
            boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          }}>
            {suggestions.map((s, idx) => (
              <div
                key={s}
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
                {s}
              </div>
            ))}
          </div>
        )}
      </div>

      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="#888" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0 }}>
        <circle cx="11" cy="11" r="8"/>
        <line x1="21" y1="21" x2="16.65" y2="16.65"/>
      </svg>

    </div>
  );
}
