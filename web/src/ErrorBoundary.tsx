import { Component, type ErrorInfo, type ReactNode } from 'react';

interface Props {
  children: ReactNode;
  // onReset is called when the user dismisses the fallback — the parent should
  // unmount/close whatever subtree crashed so re-rendering doesn't re-throw.
  onReset?: () => void;
  label?: string;
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
    return (
      <div style={overlay} role="alertdialog" aria-label="Error">
        <div style={box}>
          <h2 style={{ margin: '0 0 8px', fontSize: 16 }}>{this.props.label ?? 'Something went wrong'}</h2>
          <div style={msg}>{error.message || String(error)}</div>
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
const msg: React.CSSProperties = { fontSize: 12, color: '#f88', fontFamily: 'monospace', whiteSpace: 'pre-wrap', maxHeight: 200, overflow: 'auto', background: '#0c0c0c', border: '1px solid #333', borderRadius: 4, padding: 10 };
const btn = (primary: boolean): React.CSSProperties => ({ background: primary ? '#1d4ed8' : '#2a2a2a', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: 'pointer' });
