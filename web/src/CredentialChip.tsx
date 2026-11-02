import type { CSSProperties } from 'react';

// CredentialChip marks a stored secret, or a repo linked to one, whose expiry
// needs the reader's attention: amber while it expires within 14 days, red
// once the date has passed. Expiry is advisory (sync keeps using the secret),
// so the chip says what the date says and nothing more. Every other status
// (ok, none, absent) renders nothing.
export function CredentialChip({ status, name, testid, compact }: {
  status: string | undefined | null;
  /** The secret's name, for the hover text. */
  name?: string;
  testid?: string;
  /** The rail's rule: a fixed column whose name is the point, so the word
   *  shrinks to a mark and moves into the title. */
  compact?: boolean;
}) {
  if (status !== 'expiring' && status !== 'expired') return null;
  const word = status === 'expired' ? 'expired' : 'expiring';
  const who = name ? `Stored secret "${name}"` : 'The stored secret';
  const title = status === 'expired'
    ? `${who} has expired. Sync keeps using it until you replace it in Access.`
    : `${who} expires within 14 days. Replace it in Access.`;
  return (
    <span data-testid={testid ?? `credential-chip-${status}`} data-status={status} title={title}
      aria-label={compact ? `credential ${word}` : undefined}
      style={{ ...chip, ...TONE[status], ...(compact ? { padding: '0 4px' } : {}) }}>
      {compact ? '⚿' : word}
    </span>
  );
}

const chip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};

// Amber is "attend to this soon"; red is "the date has passed".
const TONE: Record<'expiring' | 'expired', CSSProperties> = {
  expiring: { color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22' },
  expired: { color: '#e8a0a0', background: '#1f1414', border: '1px solid #4a2626' },
};
