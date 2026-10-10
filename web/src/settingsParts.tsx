import { useState } from 'react';
import { repoHue } from './utils';
import { GitBranchIcon, CopyIcon } from './icons';

// RepoDot is the deterministic per-repo hue swatch (mirrors CreateLensForm's Dot).
export const RepoDot = ({ repo }: { repo: string }) => (
  <span style={{ width: 8, height: 8, borderRadius: '50%', background: repoHue(repo || '?'), flexShrink: 0 }} />
);

// BranchChip renders a blue git-branch chip with a mono branch label.
export const BranchChip = ({ branch }: { branch: string }) => (
  <span style={{ display: 'flex', alignItems: 'center', gap: 4, fontSize: 11.5, color: '#8af' }}>
    <GitBranchIcon color="#8af" size={11} />
    <span style={{ fontFamily: 'var(--k-font-mono)' }}>{branch}</span>
  </span>
);

// CheckMark is the small inline check glyph for a filled edit checkbox.
export const CheckMark = ({ color }: { color: string }) => (
  <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke={color} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round">
    <polyline points="20 6 9 17 4 12" />
  </svg>
);

// ConnectBody renders the "Agent access" block for a repo or a lens. It covers
// every client family we scaffold for, because they wire up differently:
//   • Claude Code uses the `kb claude init` scaffolding (skills +
//     hooks + .mcp.json).
//   • Antigravity (`agy`) uses `kb antigravity init`, which writes a
//     single owned plugin directory (.agents/plugins/knomit/).
//   • Claude Cowork, Claude Desktop, and any other stdio MCP client just
//     register kb as an mcpServers entry — no init step.
// This card is the only place in the product where the wiring is discoverable,
// so a host that ships without a row here is effectively invisible.
// The scope arg is --lens <name> for a lens, --repo <name> for a repo.
export function ConnectBody({ kind, name, agentBranch }: { kind: 'repo' | 'lens'; name: string; agentBranch?: string }) {
  const [copied, setCopied] = useState<'cc' | 'agy' | 'mcp' | null>(null);
  const arg = kind === 'lens' ? '--lens' : '--repo';
  const initCmd = `kb claude init ${arg} ${name}`;
  const agyInitCmd = `kb antigravity init ${arg} ${name}`;
  const mcpJson = `{
  "mcpServers": {
    "knomit": {
      "command": "kb",
      "args": ["${arg}", "${name}"]
    }
  }
}`;
  const endpoint = kind === 'lens'
    ? `/api/v1/lenses/${name}/mcp`
    : `/api/v1/repos/${name}/branches/${agentBranch || '<agent-branch>'}/mcp`;

  const copy = (text: string, which: 'cc' | 'agy' | 'mcp') => {
    navigator.clipboard.writeText(text)
      .then(() => { setCopied(which); setTimeout(() => setCopied(null), 1500); })
      .catch(() => { /* clipboard unavailable — nothing to recover */ });
  };

  return (
    <div data-testid={`${kind}-connect`}>
      {/* Claude Code — the init scaffolding. */}
      <div style={connectClient}>Claude Code<span style={connectHint}>scaffolds skills + hooks</span></div>
      <div style={codeRow}>
        <code style={codeText}>{initCmd}</code>
        <button type="button" data-testid={`${kind}-copy`} title="Copy" aria-label="Copy Claude Code command"
          style={copyBtn} onClick={() => copy(initCmd, 'cc')}>
          <CopyIcon color={copied === 'cc' ? '#7c9' : '#888'} size={13} />
        </button>
      </div>

      {/* Antigravity — one owned plugin directory, no merge step. */}
      <div style={{ ...connectClient, marginTop: 12 }}>Antigravity<span style={connectHint}>scaffolds a plugin directory</span></div>
      <div style={codeRow}>
        <code style={codeText}>{agyInitCmd}</code>
        <button type="button" data-testid={`${kind}-copy-agy`} title="Copy" aria-label="Copy Antigravity command"
          style={copyBtn} onClick={() => copy(agyInitCmd, 'agy')}>
          <CopyIcon color={copied === 'agy' ? '#7c9' : '#888'} size={13} />
        </button>
      </div>
      <div style={{ fontSize: 12, color: '#666', marginTop: 6 }}>
        Run <code style={{ fontFamily: 'var(--k-font-mono)', color: '#aaa' }}>agy</code> from the project directory, or pass{' '}
        <code style={{ fontFamily: 'var(--k-font-mono)', color: '#aaa' }}>--add-dir</code> on headless runs — without a registered
        workspace Antigravity loads none of it.
      </div>

      {/* Claude Cowork / Desktop / other MCP clients — raw mcpServers wiring. */}
      <div style={{ ...connectClient, marginTop: 12 }}>Cowork · Desktop · other MCP clients<span style={connectHint}>add to mcpServers config</span></div>
      <div style={{ ...codeRow, alignItems: 'flex-start' }}>
        <pre style={{ ...codeText, margin: 0, whiteSpace: 'pre', overflowX: 'auto' }}>{mcpJson}</pre>
        <button type="button" data-testid={`${kind}-copy-mcp`} title="Copy" aria-label="Copy mcpServers config"
          style={copyBtn} onClick={() => copy(mcpJson, 'mcp')}>
          <CopyIcon color={copied === 'mcp' ? '#7c9' : '#888'} size={13} />
        </button>
      </div>

      <div style={{ fontSize: 12, color: '#666', marginTop: 8 }}>
        MCP endpoint <code style={{ fontFamily: 'var(--k-font-mono)', color: '#aaa' }}>{endpoint}</code>
      </div>
    </div>
  );
}

const copyBtn: React.CSSProperties = { background: 'none', border: 'none', padding: 2, cursor: 'pointer', display: 'flex', alignItems: 'center' };

// connectClient labels a client family inside the Connect panel; connectHint is
// the muted "· what it does" trailer.
const connectClient: React.CSSProperties = { fontSize: 12, color: '#ccc', fontWeight: 600, marginBottom: 6, display: 'flex', alignItems: 'center', gap: 7 };
const connectHint: React.CSSProperties = { fontSize: 11, color: '#666', fontWeight: 400 };
// codeRow / codeText: the dark snippet box shared by both Connect blocks.
const codeRow: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, background: '#0c0c0c', border: '1px solid #222', borderRadius: 5, padding: '7px 10px' };
const codeText: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#7c9', flex: 1, lineHeight: 1.5 };
