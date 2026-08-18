// OutcomeCard reports what came back from a remote check — on the source step
// (a remote that could not be reached) and on the access step (all four probe
// outcomes). One component, because both steps are answering the same question
// and used to answer it in two different registers: an amber sentence here, a
// grey paragraph there, and nothing at all when the check SUCCEEDED.
//
// ── What it may say ──────────────────────────────────────────────────────
//
// The card states knomit's OWN actions — the URL it was given, the transport
// that scheme selects, the credential it used — because those are facts about
// this codebase. It then quotes the server verbatim, in `detail`, in full view.
// It never characterises the server's reasoning, because nothing here has
// access to it: "the host doesn't recognise that key" is a guess wearing the
// costume of an explanation, and a wrong guess sends people to fix the wrong
// thing. That is the same failure as reporting an unresolvable SSH credential
// as `unreachable`, which told users the wrong cause AND removed the access
// step that could have fixed it (internal/repos/probe.go's own comment).
//
// So callers pass a `headline` and `body` built only from probe fields, and
// `detail` straight through from ProbeResult.Detail. Nothing here interprets.
//
// ── Tones ────────────────────────────────────────────────────────────────
//
//   good  the remote answered and the wizard can proceed
//   ask   knomit needs something before it can look — a QUESTION, not a failure
//   bad   something failed: unreachable, or credentials the user supplied and
//         the host refused
//
// 'ask' is deliberately not amber. On the first probe auth_required is the
// wizard's next question, which CreateRepoWizard's own comment says outright
// ("auth_required is NOT an error here — it is the next question"), and amber
// is reserved for things that actually fail (kb/conventions/ui/copy/
// warning-styling-reserved-for-failures). Amber arrives only on the re-probe,
// where the user supplied a credential and it was rejected — the same
// distinction reprobeWithCredentials already draws in code.
export type OutcomeTone = 'good' | 'ask' | 'bad';

export function OutcomeCard({ tone, headline, url, body, detail, detailLabel, children, testid }: {
  tone: OutcomeTone;
  headline: string;
  /** The remote this is about. Omitted on the source step, where the URL field is directly above. */
  url?: string;
  body?: React.ReactNode;
  /** The server's own words, rendered verbatim. Never paraphrase into `body`. */
  detail?: string;
  /** Names who is speaking, e.g. "github.com said". */
  detailLabel?: string;
  children?: React.ReactNode;
  testid?: string;
}) {
  const t = TONES[tone];
  return (
    <div data-testid={testid ?? 'outcome-card'} data-tone={tone} style={box(t)}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 9 }}>
        <span style={{ display: 'flex', flexShrink: 0 }} aria-hidden="true"><Glyph tone={tone} color={t.head} /></span>
        <span style={{ fontSize: 13.5, fontWeight: 600, color: t.head }}>{headline}</span>
      </div>
      {url && <div style={{ ...indent, fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: t.url, wordBreak: 'break-all' }}>{url}</div>}
      {body && <div style={{ ...indent, fontSize: 12.5, lineHeight: 1.55, color: t.body, maxWidth: '64ch' }}>{body}</div>}
      {children && <div style={indent}>{children}</div>}
      {detail && (
        <div style={{ ...indent, marginTop: 10 }}>
          <div style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10, letterSpacing: 1.2, textTransform: 'uppercase', color: t.cap, marginBottom: 5 }}>
            {detailLabel ?? 'the check reported'}
          </div>
          <pre data-testid="outcome-detail" style={pre(t)}>{detail}</pre>
        </div>
      )}
    </div>
  );
}

// A glyph per tone rather than per state: the tone IS the category, and three
// shapes are what a reader can tell apart at 15px.
function Glyph({ tone, color }: { tone: OutcomeTone; color: string }) {
  const common = { width: 15, height: 15, viewBox: '0 0 24 24', fill: 'none', stroke: color, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const };
  if (tone === 'good') {
    return <svg {...common} strokeWidth="2.5"><polyline points="20 6 9 17 4 12" /></svg>;
  }
  if (tone === 'ask') {
    return (
      <svg {...common} strokeWidth="2">
        <rect x="3" y="11" width="18" height="11" rx="2" />
        <path d="M7 11V7a5 5 0 0 1 10 0v4" />
      </svg>
    );
  }
  return (
    <svg {...common} strokeWidth="2">
      <circle cx="12" cy="12" r="10" />
      <line x1="12" y1="8" x2="12" y2="12" />
      <line x1="12" y1="16" x2="12.01" y2="16" />
    </svg>
  );
}

interface Tone { bg: string; border: string; head: string; url: string; body: string; cap: string; preBg: string; preBorder: string; preText: string }

const TONES: Record<OutcomeTone, Tone> = {
  // Green, the same green the active wizard step and the write-target card use.
  good: { bg: '#121a14', border: '#26402c', head: '#9ecdae', url: '#6f9a7d', body: '#86ab92', cap: '#5d7d68', preBg: '#0a0f0b', preBorder: '#1e3324', preText: '#9ecdae' },
  // Blue — the hue this UI already spends on branches and remote refs.
  ask:  { bg: '#101720', border: '#24405e', head: '#a8c8e8', url: '#6f90ad', body: '#8aa8c2', cap: '#5f7d96', preBg: '#0a0e13', preBorder: '#1e3348', preText: '#9dbcd6' },
  // Amber, matching RepoStateChip: the thing is intact, this attempt failed.
  bad:  { bg: '#262013', border: '#4a3f22', head: '#e2c07a', url: '#b8975a', body: '#c9ad78', cap: '#8a7440', preBg: '#0d0b07', preBorder: '#33291a', preText: '#d9b978' },
};

const box = (t: Tone): React.CSSProperties => ({
  borderRadius: 6, padding: '12px 14px', background: t.bg, border: `1px solid ${t.border}`,
});
// Aligns continuation lines under the headline text rather than the glyph.
const indent: React.CSSProperties = { margin: '8px 0 0 24px' };
const pre = (t: Tone): React.CSSProperties => ({
  margin: 0, padding: '9px 11px', borderRadius: 4,
  background: t.preBg, border: `1px solid ${t.preBorder}`, color: t.preText,
  fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1.55,
  whiteSpace: 'pre-wrap', wordBreak: 'break-word',
});
