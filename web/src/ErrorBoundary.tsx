import { Component, type ErrorInfo, type ReactNode } from 'react';

interface Props {
  children: ReactNode;
  // onReset is called when the user dismisses the fallback — the parent should
  // unmount/close whatever subtree crashed so re-rendering doesn't re-throw.
  onReset?: () => void;
  label?: string;
  // 'overlay' (default) is the modal treatment for a subtree that already owns
  // the screen — the repo manager. 'inline' is for a PANEL: the fallback fills
  // only the failed pane and leaves the rest of the app usable. Wrapping a pane
  // in the overlay variant would black out the whole viewport on one bad fact
  // body, which is the opposite of what panel-level boundaries are for.
  variant?: 'overlay' | 'inline';
}

interface State {
  error: Error | null;
}

// ErrorBoundary contains render-time crashes in its subtree so a single bad
// component (e.g. a wizard hitting unexpected API data) shows a dismissible
// message instead of blanking the whole app.
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('ErrorBoundary caught:', error, info.componentStack);
  }

  private reset = () => {
    this.setState({ error: null });
    this.props.onReset?.();
  };

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    const label = this.props.label ?? 'Something went wrong';
    const detail = error.message || String(error);

    if (this.props.variant === 'inline') {
      // Contained card: fills the failed pane, scrolls its own detail, and never
      // escapes its box. "Retry" re-mounts the subtree — for a panel that is
      // usually enough (the next render reads fresh props), so it is offered
      // before the whole-app reload.
      return (
        <div style={inlineWrap} role="alert" data-testid="panel-error">
          <div style={inlineBox}>
            <div style={inlineTitle}>{label}</div>
            <div style={inlineMsg}>{detail}</div>
            <div style={inlineActions}>
              <button type="button" style={btn(false)} onClick={this.reset}>Retry</button>
              <button type="button" style={btn(true)} onClick={() => window.location.reload()}>Reload app</button>
            </div>
          </div>
        </div>
      );
    }

    return (
      <div style={overlay} role="alertdialog" aria-label="Error">
        <div style={box}>
          <h2 style={{ margin: '0 0 8px', fontSize: 16 }}>{label}</h2>
          <div style={msg}>{detail}</div>
          <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 14 }}>
            <button type="button" style={btn(false)} onClick={this.reset}>Dismiss</button>
            <button type="button" style={btn(true)} onClick={() => window.location.reload()}>Reload app</button>
          </div>
        </div>
      </div>
    );
  }
}

const overlay: React.CSSProperties = { position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)', zIndex: 2000, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 };
const box: React.CSSProperties = { background: '#161616', border: '1px solid #533', borderRadius: 8, padding: 20, width: 'min(520px, 94vw)', color: '#eee' };
const msg: React.CSSProperties = { fontSize: 12, color: '#f88', fontFamily: 'var(--k-font-mono)', whiteSpace: 'pre-wrap', maxHeight: 200, overflow: 'auto', background: '#0c0c0c', border: '1px solid #333', borderRadius: 4, padding: 10 };
const btn = (primary: boolean): React.CSSProperties => ({ background: primary ? '#1d4ed8' : '#2a2a2a', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: 'pointer' });

// Inline variant — sized to its pane (100%/100%), never fixed-positioned.
// `flex: 1` is inert outside a flex parent, so one rule serves panes that are
// flex children (library, right panel) and those that are not.
const inlineWrap: React.CSSProperties = { flex: 1, width: '100%', height: '100%', minHeight: 0, minWidth: 0, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 12, background: '#101010', overflow: 'auto', boxSizing: 'border-box' };
const inlineBox: React.CSSProperties = { background: '#161616', border: '1px solid #533', borderRadius: 6, padding: 14, maxWidth: '100%', color: '#eee' };
const inlineTitle: React.CSSProperties = { fontSize: 13, marginBottom: 8, color: '#f0b0b0' };
const inlineMsg: React.CSSProperties = { fontSize: 11, color: '#f88', fontFamily: 'var(--k-font-mono)', whiteSpace: 'pre-wrap', wordBreak: 'break-word', maxHeight: 140, overflow: 'auto', background: '#0c0c0c', border: '1px solid #333', borderRadius: 4, padding: 8 };
const inlineActions: React.CSSProperties = { display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 12 };
