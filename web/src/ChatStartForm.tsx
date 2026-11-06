import { useState } from 'react';
import type { CSSProperties, KeyboardEvent } from 'react';
import { createChat } from './chatApi';
import type { ChatInfo, ChatModelOption, ChatProvider } from './chatApi';
import { compactCount, costTier } from './chatState';
import { CHAT_ACCENT, FLEET_ACCENT } from './facets';
import { ProviderIcon } from './icons';
import {
  CHAT_NO_MODELS, COST_FREE, COST_NONE, COST_ROW_CACHE_READ, COST_ROW_INPUT, COST_ROW_OUTPUT, PROVIDER_CLOUD,
  PROVIDER_CONNECTED, PROVIDER_RUNNING, SUGGEST_BIND, SUGGEST_CHANGED, START_BUTTON, START_HEADING, START_HINT,
  START_INTRO, STAT_CONTEXT, STAT_COST, STAT_EFFORT, STAT_KEEP_ALIVE, STAT_SIZE, TAG_DEFAULT, TAG_NO_TOOLS,
  TAG_OLLAMA, TAG_TOOLS, contextNumCtx, contextTokens, perMTok, startPlaceholder, suggestContradictions,
  suggestReview, tagPriceTier,
} from './chatCopy';


/** A model can be started when the provider offers it and it can call tools. */
const usable = (m: ChatModelOption): boolean => m.available && m.tools;

const usd = (v: number): string => `$${v.toFixed(2)}`;
/** A context window in tokens: 1M, 640K, and a binary K for a multiple of 1024 that is not a round thousand (32768 is 32K). */
const windowWords = (n: number): string => (n < 1_000_000 && n % 1024 === 0 && n % 1000 !== 0 ? `${n / 1024}K` : compactCount(n));
/** Where a provider runs: the host without its scheme, or "cloud · KEY_NAME". */
function whereWords(p: ChatProvider): string {
  if (p.kind === 'ollama') return (p.host ?? '').replace(/^https?:\/\//, '');
  return p.key_source ? `${PROVIDER_CLOUD} · ${p.key_source}` : PROVIDER_CLOUD;
}

// The body of the chat DRAFT (the rail's "New chat" row): one block per
// configured provider with a card per model, and the composer. Clicking a card
// only SELECTS it; the chat is created, with its first message, by Enter or
// Start chat. A model that cannot be started (it cannot call tools, or the
// provider does not offer it) is shown dimmed with the server's reason.
export function ChatStartForm({ providers, repoNames = [], onCreated, onCancel }: {
  providers: ChatProvider[];
  /** The repos the reader has; two of them word the last suggestions. */
  repoNames?: string[];
  /** The chat exists. The caller adds it to its list and pins it in one update. */
  onCreated: (chat: ChatInfo) => void;
  onCancel?: () => void;
}) {
  // The Default model: the first listed model that can be started.
  const all = providers.flatMap(p => p.models.map(m => ({ p, m })));
  const first = all.find(x => usable(x.m));
  const [picked, setPicked] = useState<{ provider: string; model: string } | null>(null);
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  // The selection stands while its model is still startable; otherwise the Default.
  const sel = (picked && all.find(x => x.p.name === picked.provider && x.m.id === picked.model && usable(x.m))) || first;
  const selName = sel ? sel.m.name || sel.m.id : '';

  const start = async () => {
    const message = text.trim();
    if (busy || !sel || !message) return;
    setBusy(true);
    setErr('');
    try {
      const chat = await createChat(sel.p.name, sel.m.id, message);
      setBusy(false);
      onCreated(chat);
    } catch (e) {
      setBusy(false);
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      void start();
    }
  };

  const suggestions = [
    SUGGEST_CHANGED, SUGGEST_BIND,
    ...(repoNames[0] ? [suggestReview(repoNames[0])] : []),
    ...(repoNames[1] ? [suggestContradictions(repoNames[1])] : []),
  ];

  return (
    <div data-testid="chat-start-form" style={page}>
      <div style={column}>
        <h2 style={heading}>{START_HEADING}</h2>
        <p style={intro}>{START_INTRO}</p>
        {all.length === 0 && <div data-testid="chat-no-models" style={{ color: '#888', fontSize: 13 }}>{CHAT_NO_MODELS}</div>}
        <div role="radiogroup" aria-label={START_HEADING}>
          {providers.filter(p => p.models.length > 0).map(p => (
            <section key={p.name} data-testid={`chat-provider-${p.name}`} style={{ marginBottom: 20 }}>
              <div style={provHead}>
                <ProviderIcon kind={p.kind} name={p.name} color="#ccc" size={14} />
                <span style={{ fontSize: 12, fontWeight: 600, color: '#ccc' }}>{p.name}</span>
                <span data-testid={`chat-provider-where-${p.name}`} style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10.5, color: '#666' }}>{whereWords(p)}</span>
                <span data-testid={`chat-provider-reach-${p.name}`} data-reachable={p.reachable ? 'true' : 'false'} style={reach(!!p.reachable)}>
                  <i style={dot(!!p.reachable)} />
                  {p.reachable ? (p.kind === 'ollama' ? PROVIDER_RUNNING : PROVIDER_CONNECTED) : null}
                </span>
              </div>
              <div style={grid}>
                {p.models.map(m => (
                  <ModelCard
                    key={m.id}
                    provider={p}
                    model={m}
                    isDefault={first?.p === p && first.m === m}
                    selected={!!sel && sel.p === p && sel.m === m}
                    onPick={() => setPicked({ provider: p.name, model: m.id })}
                  />
                ))}
              </div>
            </section>
          ))}
        </div>

        <div style={composer}>
          <textarea
            data-testid="chat-start-input"
            value={text}
            rows={2}
            disabled={!sel}
            placeholder={selName ? startPlaceholder(selName) : ''}
            onChange={e => setText(e.target.value)}
            onKeyDown={onKey}
            style={textarea}
          />
          <div style={composerRow}>
            {sel && (
              <span data-testid="chat-start-chip" style={chip}>
                <ProviderIcon kind={sel.p.kind} name={sel.p.name} color={CHAT_ACCENT} size={11} />{selName}
              </span>
            )}
            <span style={{ fontSize: 11.5, color: '#666' }}>{START_HINT}</span>
            <button type="button" data-testid="chat-start-go" disabled={busy || !sel || !text.trim()} onClick={() => { void start(); }} style={go(busy || !sel || !text.trim())}>
              {START_BUTTON} <span aria-hidden="true">↑</span>
            </button>
          </div>
        </div>
        {err && <div role="alert" data-testid="chat-start-error" style={{ color: '#e0a0a0', fontSize: 12, marginTop: 8 }}>{err}</div>}

        {suggestions.length > 0 && (
          <div data-testid="chat-suggestions" style={sugg}>
            {suggestions.map(s => (
              <button key={s} type="button" style={suggBtn} onClick={() => setText(s)}>{s}</button>
            ))}
          </div>
        )}
        {onCancel && (
          <button type="button" data-testid="chat-cancel" disabled={busy} onClick={onCancel} style={cancelBtn}>Cancel</button>
        )}
      </div>
    </div>
  );
}

function ModelCard({ provider, model: m, isDefault, selected, onPick }: {
  provider: ChatProvider; model: ChatModelOption; isDefault: boolean; selected: boolean; onPick: () => void;
}) {
  const ok = usable(m);
  const tier = m.price?.tiers[0];
  const second = m.price && m.price.tiers.length === 2 ? m.price.tiers[0] : null;
  const ollama = provider.kind === 'ollama';
  const stats: [string, string][] = [];
  if (tier) {
    stats.push([COST_ROW_INPUT, perMTok(usd(tier.input_per_mtok))], [COST_ROW_OUTPUT, perMTok(usd(tier.output_per_mtok))]);
    if (m.price && m.price.cache_read_mult > 0) {
      stats.push([COST_ROW_CACHE_READ, `${usd(tier.input_per_mtok * m.price.cache_read_mult)} · ${m.price.cache_read_mult}×`]);
    }
  } else if (ollama) {
    stats.push([STAT_COST, COST_NONE]);
  }
  if (ollama) {
    if (m.num_ctx) stats.push([STAT_CONTEXT, contextNumCtx(windowWords(m.num_ctx))]);
    const size = [m.parameter_size, m.quantization].filter(Boolean).join(' · ');
    if (size) stats.push([STAT_SIZE, size]);
    if (m.keep_alive) stats.push([STAT_KEEP_ALIVE, m.keep_alive]);
  } else {
    if (m.context_window) stats.push([STAT_CONTEXT, contextTokens(windowWords(m.context_window))]);
    if (m.effort) stats.push([STAT_EFFORT, m.effort]);
  }
  const mark = tier ? costTier(tier.input_per_mtok) : null;
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      data-testid={`chat-model-${provider.name}-${m.id}`}
      data-selected={selected ? 'true' : 'false'}
      disabled={!ok}
      onClick={() => { if (ok) onPick(); }}
      title={m.id}
      style={card(selected, ok)}
    >
      <span style={radio(selected)} aria-hidden="true">{selected && <i style={radioDot} />}</span>
      <span style={{ fontSize: 14, fontWeight: 600, color: '#eee', paddingRight: 28 }}>{m.name || m.id}</span>
      <span data-testid={`chat-model-id-${provider.name}-${m.id}`} style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10.5, color: '#6a6a6a', marginTop: -6, overflowWrap: 'anywhere' }}>{m.id}</span>
      {mark !== null && (
        <span data-testid={`chat-model-cost-${provider.name}-${m.id}`} style={costMark} aria-label={'$'.repeat(mark)}>
          {'$'.repeat(mark)}<s style={{ color: '#3a3a3a', textDecoration: 'none' }}>{'$'.repeat(3 - mark)}</s>
        </span>
      )}
      {mark === null && ollama && <span style={{ ...costMark, color: FLEET_ACCENT, letterSpacing: 0 }}>{COST_FREE}</span>}
      {m.best_for && <span data-testid={`chat-model-bestfor-${provider.name}-${m.id}`} style={bestFor}>{m.best_for}</span>}
      {!ok && m.reason && <span data-testid={`chat-model-reason-${provider.name}-${m.id}`} style={bestFor}>{m.reason}</span>}
      <span style={{ display: 'flex', gap: 5, flexWrap: 'wrap' }}>
        {isDefault && <span style={{ ...tag, color: CHAT_ACCENT, borderColor: '#4a3f22', background: '#1d1a12' }}>{TAG_DEFAULT}</span>}
        {ok && <span style={tag}>{TAG_TOOLS}</span>}
        {second && <span style={tag}>{tagPriceTier(compactCount(second.up_to_prompt_tokens))}</span>}
        {ollama && <span style={{ ...tag, color: FLEET_ACCENT, borderColor: '#24443f', background: '#11201d' }}>{TAG_OLLAMA}</span>}
        {m.available && !m.tools && <span style={{ ...tag, color: '#e0a0a0', borderColor: '#4a2626', background: '#1f1414' }}>{TAG_NO_TOOLS}</span>}
      </span>
      {stats.length > 0 && (
        <span style={statsGrid}>
          {stats.map(([k, v]) => (
            <span key={k} style={{ display: 'contents' }}>
              <span style={{ color: '#6a6a6a' }}>{k}</span>
              <span style={{ color: '#c8c8c8', fontFamily: 'var(--k-font-mono)', fontSize: 10.5, textAlign: 'right' }}>{v}</span>
            </span>
          ))}
        </span>
      )}
    </button>
  );
}

const page: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '30px 24px 24px', display: 'flex', justifyContent: 'center' };
const column: CSSProperties = { width: 900, maxWidth: '100%' };
const heading: CSSProperties = { margin: '0 0 4px', fontSize: 22, fontWeight: 600, color: '#eee', letterSpacing: '-0.2px', fontFamily: 'var(--k-font-display, inherit)' };
const intro: CSSProperties = { margin: '0 0 22px', color: '#888', fontSize: 13 };
const provHead: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, marginBottom: 9 };
const reach = (on: boolean): CSSProperties => ({ marginLeft: 'auto', fontSize: 11, color: on ? '#77cc99' : '#666', display: 'flex', alignItems: 'center', gap: 5 });
const dot = (on: boolean): CSSProperties => ({ width: 6, height: 6, borderRadius: '50%', background: on ? '#77cc99' : '#555', display: 'inline-block' });
const grid: CSSProperties = { display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 240px), 1fr))', gap: 10 };
const card = (selected: boolean, ok: boolean): CSSProperties => ({
  position: 'relative', textAlign: 'left', font: 'inherit', display: 'flex', flexDirection: 'column', gap: 9,
  padding: '12px 13px 11px', borderRadius: 8, cursor: ok ? 'pointer' : 'default',
  border: `1px solid ${selected ? '#6b5628' : '#262626'}`,
  background: selected ? '#17140e' : '#121212',
  boxShadow: selected ? '0 0 0 3px rgba(232,194,122,.08)' : 'none',
  opacity: ok ? 1 : 0.5, color: '#ddd', minWidth: 0,
});
const radio = (on: boolean): CSSProperties => ({
  position: 'absolute', top: 12, right: 12, width: 14, height: 14, borderRadius: '50%', boxSizing: 'border-box',
  border: `1.5px solid ${on ? CHAT_ACCENT : '#555'}`, display: 'flex', alignItems: 'center', justifyContent: 'center',
});
const radioDot: CSSProperties = { width: 6, height: 6, borderRadius: '50%', background: CHAT_ACCENT, display: 'block' };
const costMark: CSSProperties = { position: 'absolute', right: 13, top: 38, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: CHAT_ACCENT, letterSpacing: 1 };
const bestFor: CSSProperties = { fontSize: 12, color: '#aaa', lineHeight: 1.45 };
const tag: CSSProperties = { fontSize: 10, padding: '1px 6px', borderRadius: 3, border: '1px solid #2c2c2c', color: '#999', background: '#161616' };
const statsGrid: CSSProperties = { display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '3px 12px', fontSize: 11, borderTop: '1px solid #222', paddingTop: 8 };
const composer: CSSProperties = { marginTop: 4, border: '1px solid #333', background: '#151515', borderRadius: 10, padding: '10px 12px 9px' };
const textarea: CSSProperties = {
  width: '100%', boxSizing: 'border-box', resize: 'none', background: 'transparent', border: 'none', outline: 'none',
  color: '#e4e4e4', font: 'inherit', fontSize: 14, minHeight: 38, marginBottom: 8,
};
const composerRow: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' };
const chip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 11.5, color: CHAT_ACCENT, background: '#1d1a12',
  border: '1px solid #4a3f22', borderRadius: 12, padding: '2px 9px',
};
const go = (disabled: boolean): CSSProperties => ({
  marginLeft: 'auto', display: 'inline-flex', alignItems: 'center', gap: 6, background: CHAT_ACCENT, color: '#1a1408',
  fontWeight: 600, fontSize: 12.5, borderRadius: 6, padding: '6px 13px', border: 'none',
  cursor: disabled ? 'default' : 'pointer', opacity: disabled ? 0.45 : 1,
});
const sugg: CSSProperties = { display: 'flex', gap: 8, marginTop: 10, flexWrap: 'wrap' };
const suggBtn: CSSProperties = { fontSize: 12, color: '#aaa', border: '1px solid #262626', borderRadius: 14, padding: '4px 11px', background: '#111', cursor: 'pointer', font: 'inherit' };
const cancelBtn: CSSProperties = {
  marginTop: 14, background: 'transparent', color: '#aaa', border: '1px solid #333',
  borderRadius: 4, padding: '6px 12px', fontSize: 12, cursor: 'pointer',
};
