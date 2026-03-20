import { useState, useRef, useCallback } from 'react';
import { createSession, streamTest, streamPreview, streamApply, streamCommit, deleteSession } from './api';
import type { SSEEvent, TestResult, PreviewResult, ApplyResult } from './api';

type Step =
  | 'idle'
  | 'creating'
  | 'testing'
  | 'tested'
  | 'previewing'
  | 'previewed'
  | 'applying'
  | 'applied'
  | 'committing'
  | 'done';

interface Props {
  repo: string;
  onClose: () => void;
}

export function ConnectRemoteModal({ repo, onClose }: Props) {
  // Form state
  const [url, setUrl] = useState('');
  const [authMethod, setAuthMethod] = useState<'' | 'ssh' | 'token' | 'basic'>('');
  const [token, setToken] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');

  // Workflow state
  const [step, setStep] = useState<Step>('idle');
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<TestResult | null>(null);
  const [previewResult, setPreviewResult] = useState<PreviewResult | null>(null);
  const [strategy, setStrategy] = useState<'local' | 'remote'>('local');
  const [selectedBranch, setSelectedBranch] = useState<string>('');
  const [applyResult, setApplyResult] = useState<ApplyResult | null>(null);
  const [progress, setProgress] = useState('');
  const [error, setError] = useState<{ section: Step; message: string } | null>(null);

  const cleanupRef = useRef<(() => void) | null>(null);

  const handleCancel = useCallback(() => {
    if (cleanupRef.current) { cleanupRef.current(); cleanupRef.current = null; }
    if (sessionId) { deleteSession(repo, sessionId).catch(() => {}); }
    onClose();
  }, [repo, sessionId, onClose]);

  const handleTest = async () => {
    setError(null);
    setStep('creating');
    setProgress('Creating session...');
    try {
      const sess = await createSession(repo, {
        url,
        auth_method: authMethod || undefined,
        token: authMethod === 'token' ? token : undefined,
        user: authMethod === 'basic' ? user : undefined,
        password: authMethod === 'basic' ? password : undefined,
      });
      setSessionId(sess.session_id);
      setStep('testing');
      setProgress('Connecting...');

      await new Promise<void>((resolve, reject) => {
        const close = streamTest(repo, sess.session_id, (ev: SSEEvent) => {
          if (ev.phase === 'done') {
            const result = ev.result as TestResult;
            setTestResult(result);
            setSelectedBranch(result.default_branch);
            setStep('tested');
            setProgress('');
            resolve();
            // Auto-trigger preview
            startPreview(sess.session_id);
          } else if (ev.phase === 'error') {
            setError({ section: 'testing', message: ev.message });
            setStep('tested');
            reject(new Error(ev.message));
          } else if (ev.phase === 'cloning') {
            setProgress(ev.progress || 'Cloning...');
          } else {
            setProgress(ev.phase + '...');
          }
        });
        cleanupRef.current = close;
      });
    } catch (e: any) {
      if (!error) setError({ section: 'creating', message: e.message || 'Failed to create session' });
      if (step === 'creating') setStep('idle');
    }
  };

  const startPreview = async (sid: string) => {
    setError(null);
    setStep('previewing');
    setProgress('Analyzing...');
    try {
      await new Promise<void>((resolve, reject) => {
        const close = streamPreview(repo, sid, (ev: SSEEvent) => {
          if (ev.phase === 'done') {
            setPreviewResult(ev.result as PreviewResult);
            setStep('previewed');
            setProgress('');
            resolve();
          } else if (ev.phase === 'error') {
            setError({ section: 'previewing', message: ev.message });
            setStep('previewed');
            reject(new Error(ev.message));
          } else if (ev.phase === 'comparing') {
            setProgress('Comparing facts...');
          } else {
            setProgress(ev.phase + '...');
          }
        });
        cleanupRef.current = close;
      });
    } catch (e: any) {
      if (!error) setError({ section: 'previewing', message: e.message || 'Preview failed' });
    }
  };

  const handleApply = async () => {
    if (!sessionId) return;
    setError(null);
    setApplyResult(null);
    setStep('applying');
    setProgress('Merging...');
    try {
      await streamApply(repo, sessionId, strategy === 'local' ? 'local_wins' : 'remote_wins', selectedBranch || undefined, (ev: SSEEvent) => {
        if (ev.phase === 'done') {
          setApplyResult(ev.result as ApplyResult);
          setStep('applied');
          setProgress('');
        } else if (ev.phase === 'error') {
          setError({ section: 'applying', message: ev.message });
          setStep('applied');
        } else if (ev.phase === 'replaying') {
          setProgress(`Replaying ${(ev as any).current}/${(ev as any).total}...`);
        } else if (ev.phase === 'merging') {
          setProgress('Merging...');
        } else {
          setProgress(ev.phase + '...');
        }
      });
    } catch (e: any) {
      setError({ section: 'applying', message: e.message || 'Apply failed' });
      setStep('applied');
    }
  };

  const handleCommit = async () => {
    if (!sessionId) return;
    setError(null);
    setStep('committing');
    setProgress('Finalizing...');
    try {
      await streamCommit(repo, sessionId, (ev: SSEEvent) => {
        if (ev.phase === 'done') {
          setStep('done');
          setProgress('');
          setTimeout(() => onClose(), 1200);
        } else if (ev.phase === 'error') {
          setError({ section: 'committing', message: ev.message });
        } else if (ev.phase === 'swapping') {
          setProgress('Swapping store...');
        } else if (ev.phase === 'configuring') {
          setProgress('Configuring remote...');
        } else {
          setProgress(ev.phase + '...');
        }
      });
    } catch (e: any) {
      setError({ section: 'committing', message: e.message || 'Commit failed' });
    }
  };

  const handleRetry = () => {
    if (!error) return;
    const section = error.section;
    setError(null);
    if (section === 'creating' || section === 'testing') {
      setStep('idle');
      setTestResult(null);
      setPreviewResult(null);
      setApplyResult(null);
      if (sessionId) { deleteSession(repo, sessionId).catch(() => {}); setSessionId(null); }
    } else if (section === 'previewing') {
      if (sessionId) startPreview(sessionId);
    } else if (section === 'applying') {
      setStep('previewed');
      setApplyResult(null);
    } else if (section === 'committing') {
      setStep('applied');
    }
  };

  const handleTryDifferentStrategy = () => {
    setApplyResult(null);
    setStep('previewed');
  };

  const isSSHURL = url.startsWith('git@') || url.startsWith('ssh://');
  const isHTTPURL = url.startsWith('http://') || url.startsWith('https://');
  const authMismatch = (isHTTPURL && authMethod === 'ssh')
    ? 'SSH auth cannot be used with HTTP/HTTPS URLs'
    : (isSSHURL && (authMethod === 'token' || authMethod === 'basic'))
    ? 'Token/basic auth cannot be used with SSH URLs'
    : '';

  const canTest = url && !authMismatch && step === 'idle';
  const busy = step === 'creating' || step === 'testing' || step === 'previewing' || step === 'applying' || step === 'committing';

  const overlay: React.CSSProperties = {
    position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
    background: 'rgba(0,0,0,0.6)', display: 'flex', alignItems: 'center',
    justifyContent: 'center', zIndex: 1000,
  };

  const modal: React.CSSProperties = {
    background: '#0d0d0d', border: '1px solid #222', borderRadius: 8,
    padding: 24, width: 520, maxWidth: '90vw', maxHeight: '80vh',
    overflowY: 'auto', color: '#eee', fontFamily: 'system-ui, sans-serif',
  };

  const label: React.CSSProperties = {
    fontSize: 12, color: '#888', marginBottom: 4, display: 'block',
  };

  const input: React.CSSProperties = {
    width: '100%', boxSizing: 'border-box' as const, background: '#1a1a1a',
    border: '1px solid #333', color: '#eee', padding: '6px 8px',
    borderRadius: 4, fontSize: 13, marginBottom: 12,
  };

  const btn = (disabled: boolean, variant: 'primary' | 'secondary' | 'danger' = 'primary'): React.CSSProperties => ({
    padding: '6px 16px', borderRadius: 4, border: 'none', fontSize: 13,
    cursor: disabled ? 'not-allowed' : 'pointer',
    background: disabled ? '#333' : variant === 'primary' ? '#2563eb' : variant === 'danger' ? '#b91c1c' : '#444',
    color: disabled ? '#666' : '#fff',
    marginRight: 8,
  });

  const sectionBox: React.CSSProperties = {
    marginTop: 16, padding: 12, background: '#111', borderRadius: 4, fontSize: 13,
  };

  const stepPassed = (s: Step) => {
    const order: Step[] = ['idle', 'creating', 'testing', 'tested', 'previewing', 'previewed', 'applying', 'applied', 'committing', 'done'];
    return order.indexOf(step) >= order.indexOf(s);
  };

  return (
    <div style={overlay} onClick={handleCancel}>
      <div style={modal} onClick={e => e.stopPropagation()}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
          <h2 style={{ margin: 0, fontSize: 16 }}>Connect Remote</h2>
          <button onClick={handleCancel} style={{ background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 16 }}>x</button>
        </div>

        {/* Section 1: URL + Auth */}
        <label style={label}>Remote URL</label>
        <input
          style={{ ...input, opacity: busy ? 0.5 : 1 }}
          value={url}
          onChange={e => setUrl(e.target.value)}
          placeholder="git@github.com:user/repo.git"
          disabled={busy || stepPassed('tested')}
        />

        <label style={label}>Auth Method</label>
        <select
          value={authMethod}
          onChange={e => setAuthMethod(e.target.value as typeof authMethod)}
          style={{ ...input, cursor: busy || stepPassed('tested') ? 'not-allowed' : 'pointer', opacity: busy ? 0.5 : 1 }}
          disabled={busy || stepPassed('tested')}
        >
          <option value="">None</option>
          <option value="ssh">SSH (knomit key)</option>
          <option value="token">Token</option>
          <option value="basic">Basic (user/password)</option>
        </select>

        {authMethod === 'token' && (
          <>
            <label style={label}>Token</label>
            <input style={{ ...input, opacity: busy ? 0.5 : 1 }} type="password" value={token} onChange={e => setToken(e.target.value)} placeholder="ghp_..." disabled={busy || stepPassed('tested')} />
          </>
        )}

        {authMethod === 'basic' && (
          <>
            <label style={label}>Username</label>
            <input style={{ ...input, opacity: busy ? 0.5 : 1 }} value={user} onChange={e => setUser(e.target.value)} disabled={busy || stepPassed('tested')} />
            <label style={label}>Password</label>
            <input style={{ ...input, opacity: busy ? 0.5 : 1 }} type="password" value={password} onChange={e => setPassword(e.target.value)} disabled={busy || stepPassed('tested')} />
          </>
        )}

        {authMismatch && <div style={{ color: '#f44336', fontSize: 12, marginBottom: 8 }}>{authMismatch}</div>}

        {step === 'idle' && (
          <button disabled={!canTest} onClick={handleTest} style={btn(!canTest)}>
            Test Connection
          </button>
        )}

        {(step === 'creating' || step === 'testing') && (
          <div style={{ fontSize: 13, color: '#8af', marginTop: 8 }}>{progress}</div>
        )}

        {error && error.section === 'creating' && (
          <div style={sectionBox}>
            <div style={{ color: '#f44336' }}>{error.message}</div>
            <button onClick={handleRetry} style={{ ...btn(false), marginTop: 8 }}>Retry</button>
          </div>
        )}

        {/* Section 2: Test result */}
        {stepPassed('tested') && testResult && !error?.section?.startsWith('test') && (
          <div style={sectionBox}>
            <div style={{ color: '#4caf50', marginBottom: 6 }}>Connected</div>
            <div style={{ color: '#aaa' }}>
              <span>{testResult.remote_fact_count} facts on remote</span>
              <span style={{ margin: '0 8px', color: '#444' }}>|</span>
              <span>{testResult.local_fact_count} local</span>
            </div>
            <div style={{ color: '#888', marginTop: 4, display: 'flex', alignItems: 'center', gap: 8 }}>
              <span>Remote branch:</span>
              {testResult.branches.length <= 1 ? (
                <span>{selectedBranch || testResult.default_branch}</span>
              ) : (
                <select
                  value={selectedBranch}
                  onChange={e => setSelectedBranch(e.target.value)}
                  disabled={busy}
                  style={{ background: '#1a1a1a', border: '1px solid #333', color: '#eee', padding: '2px 6px', borderRadius: 4, fontSize: 13 }}
                >
                  {testResult.branches.map(b => (
                    <option key={b} value={b}>{b}</option>
                  ))}
                </select>
              )}
            </div>
            <div style={{ color: '#888', marginTop: 2 }}>
              Histories: {testResult.history}
            </div>
          </div>
        )}

        {error && error.section === 'testing' && (
          <div style={sectionBox}>
            <div style={{ color: '#f44336' }}>{error.message}</div>
            <button onClick={handleRetry} style={{ ...btn(false), marginTop: 8 }}>Retry</button>
          </div>
        )}

        {/* Section 3: Preview */}
        {step === 'previewing' && (
          <div style={sectionBox}>
            <div style={{ color: '#8af' }}>{progress}</div>
          </div>
        )}

        {stepPassed('previewed') && previewResult && !error?.section?.startsWith('preview') && (
          <div style={sectionBox}>
            <div style={{ color: '#aaa' }}>
              {previewResult.local_only} local-only
              <span style={{ margin: '0 6px', color: '#444' }}>&middot;</span>
              {previewResult.remote_only} remote-only
              <span style={{ margin: '0 6px', color: '#444' }}>&middot;</span>
              {previewResult.shared_path} shared paths
            </div>
            {(previewResult.dead_refs_found > 0) && (
              <div style={{ color: '#888', marginTop: 4 }}>
                {previewResult.dead_refs_found} dead refs found
              </div>
            )}
          </div>
        )}

        {error && error.section === 'previewing' && (
          <div style={sectionBox}>
            <div style={{ color: '#f44336' }}>{error.message}</div>
            <button onClick={handleRetry} style={{ ...btn(false), marginTop: 8 }}>Retry</button>
          </div>
        )}

        {/* Section 4: Strategy + Apply */}
        {stepPassed('previewed') && !error?.section?.startsWith('preview') && step !== 'applied' && step !== 'committing' && step !== 'done' && (
          <div style={sectionBox}>
            <div style={{ marginBottom: 8, color: '#888', fontSize: 12 }}>Conflict strategy</div>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: step === 'applying' ? 'not-allowed' : 'pointer', color: '#ccc', fontSize: 13, marginBottom: 6 }}>
              <input type="radio" name="strategy" checked={strategy === 'local'} onChange={() => setStrategy('local')} disabled={step === 'applying'} />
              Local wins
            </label>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: step === 'applying' ? 'not-allowed' : 'pointer', color: '#ccc', fontSize: 13, marginBottom: 10 }}>
              <input type="radio" name="strategy" checked={strategy === 'remote'} onChange={() => setStrategy('remote')} disabled={step === 'applying'} />
              Remote wins
            </label>

            {step === 'applying' && (
              <div style={{ color: '#8af', marginBottom: 8 }}>{progress}</div>
            )}

            {step !== 'applying' && (
              <button onClick={handleApply} style={btn(false)}>
                Preview Merge
              </button>
            )}
          </div>
        )}

        {error && error.section === 'applying' && (
          <div style={sectionBox}>
            <div style={{ color: '#f44336' }}>{error.message}</div>
            <button onClick={handleRetry} style={{ ...btn(false), marginTop: 8 }}>Retry</button>
          </div>
        )}

        {/* Apply result + Commit */}
        {step === 'applied' && applyResult && !error && (
          <div style={sectionBox}>
            <div style={{ color: '#4caf50', marginBottom: 6 }}>Merge complete</div>
            <div style={{ color: '#aaa' }}>
              {applyResult.total_facts} total facts: {applyResult.from_local} from local, {applyResult.from_remote} from remote
            </div>
            {applyResult.overwrites > 0 && (
              <div style={{ color: '#888', marginTop: 2 }}>{applyResult.overwrites} overwrites ({strategy === 'local' ? 'local wins' : 'remote wins'})</div>
            )}
            {applyResult.refs_resolved_from_history > 0 && (
              <div style={{ color: '#888', marginTop: 2 }}>{applyResult.refs_resolved_from_history} refs resolved from history</div>
            )}
            {applyResult.dangling_refs_dropped > 0 && (
              <div style={{ color: '#888', marginTop: 2 }}>{applyResult.dangling_refs_dropped} dangling refs dropped</div>
            )}
            <div style={{ display: 'flex', marginTop: 12, gap: 8 }}>
              <button onClick={handleTryDifferentStrategy} style={btn(false, 'secondary')}>
                Try Different Strategy
              </button>
              <button onClick={handleCommit} style={btn(false)}>
                Apply
              </button>
            </div>
          </div>
        )}

        {/* Section 5: Committing */}
        {step === 'committing' && (
          <div style={sectionBox}>
            <div style={{ color: '#8af' }}>{progress}</div>
          </div>
        )}

        {error && error.section === 'committing' && (
          <div style={sectionBox}>
            <div style={{ color: '#f44336' }}>{error.message}</div>
            <button onClick={handleRetry} style={{ ...btn(false), marginTop: 8 }}>Retry</button>
          </div>
        )}

        {step === 'done' && (
          <div style={sectionBox}>
            <div style={{ color: '#4caf50' }}>Remote connected successfully.</div>
          </div>
        )}

        {/* Cancel always available at bottom when busy */}
        {busy && (
          <div style={{ marginTop: 16, textAlign: 'right' }}>
            <button onClick={handleCancel} style={btn(false, 'danger')}>Cancel</button>
          </div>
        )}
      </div>
    </div>
  );
}
