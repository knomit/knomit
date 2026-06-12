import { useState, useRef, useCallback, useEffect } from 'react';
import { api, createSession, streamTest, streamPreview, streamApply, streamCommit, deleteSession } from './api';
import type { SSEEvent, TestResult, PreviewResult, ApplyResult } from './api';

type Step =
  | 'idle' | 'creating' | 'testing' | 'tested'
  | 'previewing' | 'previewed' | 'applying' | 'applied'
  | 'committing' | 'done';

interface Props {
  repo: string;
  onCancel: () => void;   // user backed out — return to the manager
  onDone: () => void;     // remote connected — return to the manager
}

// RemoteConnectWizard is the stepped connect/reconcile flow shown as the full
// body of the Repo Manager dialog. It drives the session backend
// (test → preview → apply → commit) and presents three steps:
//   ① Connect   ② Review   ③ Sync
export function RemoteConnectWizard({ repo, onCancel, onDone }: Props) {
  const [url, setUrl] = useState('');
  const [authMethod, setAuthMethod] = useState<'' | 'ssh' | 'token' | 'basic'>('');
  const [token, setToken] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');

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
  useEffect(() => () => { cleanupRef.current?.(); }, []);

  // Prefill from the existing origin (reconnect/change case). The secret is
  // never returned by the API, so token/password start blank.
  useEffect(() => {
    let cancelled = false;
    api.getOrigin(repo).then(o => {
      if (cancelled || !o) return;
      if (o.url) setUrl(o.url);
      const m = o.auth_method;
      if (m === 'ssh' || m === 'token' || m === 'basic') setAuthMethod(m);
    }).catch(() => { /* leave blank */ });
    return () => { cancelled = true; };
  }, [repo]);

  const cancel = useCallback(() => {
    cleanupRef.current?.(); cleanupRef.current = null;
    if (sessionId) deleteSession(repo, sessionId).catch(() => {});
    onCancel();
  }, [repo, sessionId, onCancel]);

  const handleTest = async () => {
    setError(null); setStep('creating'); setProgress('Creating session…');
    try {
      const sess = await createSession(repo, {
        url,
        auth_method: authMethod || undefined,
        token: authMethod === 'token' ? token : undefined,
        user: authMethod === 'basic' ? user : undefined,
        password: authMethod === 'basic' ? password : undefined,
      });
      setSessionId(sess.session_id);
      setStep('testing'); setProgress('Connecting…');
      await new Promise<void>((resolve, reject) => {
        const close = streamTest(repo, sess.session_id, (ev: SSEEvent) => {
          if (ev.phase === 'done') {
            const result = ev.result as TestResult;
            setTestResult(result); setSelectedBranch(result.default_branch || '');
            setStep('tested'); setProgress(''); resolve();
            startPreview(sess.session_id);
          } else if (ev.phase === 'error') {
            setError({ section: 'testing', message: ev.message }); setStep('idle'); reject(new Error(ev.message));
          } else if (ev.phase === 'cloning') {
            setProgress(ev.progress || 'Cloning…');
          } else { setProgress(ev.phase + '…'); }
        });
        cleanupRef.current = close;
      });
    } catch (e) {
      if (!error) setError({ section: 'creating', message: (e instanceof Error && e.message) || 'Failed to create session' });
      if (step === 'creating') setStep('idle');
    }
  };

  const startPreview = async (sid: string) => {
    setError(null); setStep('previewing'); setProgress('Analyzing…');
    try {
      await new Promise<void>((resolve, reject) => {
        const close = streamPreview(repo, sid, (ev: SSEEvent) => {
          if (ev.phase === 'done') {
            setPreviewResult(ev.result as PreviewResult); setStep('previewed'); setProgress(''); resolve();
          } else if (ev.phase === 'error') {
            setError({ section: 'previewing', message: ev.message }); setStep('previewed'); reject(new Error(ev.message));
          } else if (ev.phase === 'comparing') {
            setProgress('Comparing facts…');
          } else { setProgress(ev.phase + '…'); }
        });
        cleanupRef.current = close;
      });
    } catch (e) {
      if (!error) setError({ section: 'previewing', message: (e instanceof Error && e.message) || 'Preview failed' });
    }
  };

  const handleApply = async () => {
    if (!sessionId) return;
    setError(null); setApplyResult(null); setStep('applying'); setProgress('Merging…');
    try {
      await streamApply(repo, sessionId, strategy === 'local' ? 'local_wins' : 'remote_wins', selectedBranch || undefined, (ev: SSEEvent) => {
        if (ev.phase === 'done') {
          setApplyResult(ev.result as ApplyResult); setStep('applied'); setProgress('');
        } else if (ev.phase === 'error') {
          setError({ section: 'applying', message: ev.message }); setStep('applied');
        } else if (ev.phase === 'replaying') {
          setProgress(`Replaying ${ev.current}/${ev.total}…`);
        } else if (ev.phase === 'merging') {
          setProgress('Merging…');
        } else { setProgress(ev.phase + '…'); }
      });
    } catch (e) {
      setError({ section: 'applying', message: (e instanceof Error && e.message) || 'Apply failed' }); setStep('applied');
    }
  };

  const handleCommit = async () => {
    if (!sessionId) return;
    setError(null); setStep('committing'); setProgress('Finalizing…');
    try {
      await streamCommit(repo, sessionId, (ev: SSEEvent) => {
        if (ev.phase === 'done') {
          setStep('done'); setProgress(''); setTimeout(() => onDone(), 1200);
        } else if (ev.phase === 'error') {
          setError({ section: 'committing', message: ev.message });
        } else if (ev.phase === 'swapping') {
          setProgress('Swapping store…');
        } else if (ev.phase === 'configuring') {
          setProgress('Configuring remote…');
        } else if (ev.phase === 'rebuilding') {
          const sub = ev.sub_phase || ''; const cur = ev.current || 0; const tot = ev.total || 0;
          setProgress(tot > 0 ? `Rebuilding ${sub}… ${cur}/${tot}` : 'Rebuilding index…');
        } else { setProgress(ev.phase + '…'); }
      });
    } catch (e) {
      setError({ section: 'committing', message: (e instanceof Error && e.message) || 'Commit failed' });
    }
  };

  const handleRetry = () => {
    if (!error) return;
    const section = error.section; setError(null);
    if (section === 'creating' || section === 'testing') {
      setStep('idle'); setTestResult(null); setPreviewResult(null); setApplyResult(null);
      if (sessionId) { deleteSession(repo, sessionId).catch(() => {}); setSessionId(null); }
    } else if (section === 'previewing') {
      if (sessionId) startPreview(sessionId);
    } else if (section === 'applying') {
      setStep('previewed'); setApplyResult(null);
    } else if (section === 'committing') {
      setStep('applied');
    }
  };

  const handleBack = () => {
    setStep('idle'); setTestResult(null); setPreviewResult(null); setApplyResult(null); setError(null);
    if (sessionId) { deleteSession(repo, sessionId).catch(() => {}); setSessionId(null); }
  };

  const isSSHURL = url.startsWith('git@') || url.startsWith('ssh://');
  const isHTTPURL = url.startsWith('http://') || url.startsWith('https://');
  const authMismatch = (isHTTPURL && authMethod === 'ssh') ? 'SSH auth cannot be used with HTTP/HTTPS URLs'
    : (isSSHURL && (authMethod === 'token' || authMethod === 'basic')) ? 'Token/basic auth cannot be used with SSH URLs' : '';
  const canTest = !!url && !authMismatch && step === 'idle';
  const busy = step === 'creating' || step === 'testing' || step === 'previewing' || step === 'applying' || step === 'committing';
  const isSharedHistory = testResult?.history === 'shared';

  const onReview = testResult && step !== 'idle' && step !== 'creating' && step !== 'testing'
    && step !== 'committing' && step !== 'done';
  const activeStep: 1 | 2 | 3 = (step === 'committing' || step === 'done') ? 3 : onReview ? 2 : 1;

  return (
    <div data-testid="remote-connect-wizard" style={wrap}>
      <header style={head}>
        <h2 style={{ margin: 0, fontSize: 16 }}>Connect remote — {repo}</h2>
        {step !== 'committing' && step !== 'done' && (
          <button type="button" data-testid="wizard-close" style={iconBtn} onClick={cancel} aria-label="Cancel">✕</button>
        )}
      </header>

      <div style={steps}>
        {([[1, 'Connect'], [2, 'Review'], [3, 'Sync']] as const).map(([n, lbl]) => {
          const state = activeStep === n ? 'active' : activeStep > n ? 'done' : 'todo';
          return (
            <div key={n} style={stepItem}>
              <span style={stepDot(state)}>{state === 'done' ? '✓' : n}</span>
              <span style={{ color: state === 'todo' ? '#666' : '#ddd', fontSize: 13 }}>{lbl}</span>
              {n < 3 && <span style={stepBar(activeStep > n)} />}
            </div>
          );
        })}
      </div>

      <div style={bodyArea}>
        {/* ── Step ① Connect ── */}
        {activeStep === 1 && (
          <>
            <label style={label}>Remote URL</label>
            <input data-testid="wizard-url" style={input} value={url} disabled={busy} placeholder="git@github.com:user/repo.git"
              onChange={e => setUrl(e.target.value)} />
            <label style={label}>Auth method</label>
            <select style={input} value={authMethod} disabled={busy} onChange={e => setAuthMethod(e.target.value as typeof authMethod)}>
              <option value="">None</option>
              <option value="ssh">SSH (knomit key)</option>
              <option value="token">Token</option>
              <option value="basic">Basic (user / password)</option>
            </select>
            {authMethod === 'token' && (<>
              <label style={label}>Token</label>
              <input style={input} type="password" value={token} disabled={busy} placeholder="ghp_…" onChange={e => setToken(e.target.value)} />
            </>)}
            {authMethod === 'basic' && (<>
              <label style={label}>Username</label>
              <input style={input} value={user} disabled={busy} onChange={e => setUser(e.target.value)} />
              <label style={label}>Password</label>
              <input style={input} type="password" value={password} disabled={busy} onChange={e => setPassword(e.target.value)} />
            </>)}
            {authMismatch && <div style={errText}>{authMismatch}</div>}
            {(step === 'creating' || step === 'testing') && <div style={progressText}>{progress}</div>}
            {error && (step === 'idle') && (
              <div style={errBox}><div style={{ color: '#f88' }}>{error.message}</div>
                <button type="button" style={btn(false)} onClick={handleRetry}>Retry</button></div>
            )}
          </>
        )}

        {/* ── Step ② Review ── */}
        {activeStep === 2 && testResult && (
          <>
            <div style={{ fontSize: 12, color: '#888', marginBottom: 12 }}>
              {url} — {testResult.remote_fact_count} remote · {testResult.local_fact_count} local · {testResult.history} histories
            </div>
            <div style={{ display: 'flex', gap: 16, marginBottom: 12, fontSize: 13, alignItems: 'center' }}>
              <span style={{ color: '#888' }}>Branch:</span>
              <select value={selectedBranch} disabled={busy || (testResult.branches ?? []).length === 0} onChange={e => setSelectedBranch(e.target.value)}
                style={{ background: '#1a1a1a', border: '1px solid #333', color: '#eee', padding: '2px 6px', borderRadius: 4, fontSize: 13 }}>
                {(testResult.branches ?? []).length === 0
                  ? <option value="">(no remote branches yet)</option>
                  : (testResult.branches ?? []).map(b => <option key={b} value={b}>{b}</option>)}
              </select>
              {testResult.matched_agent && <span style={{ color: '#8af' }}>agent branch found — will replay on top</span>}
            </div>

            {step === 'previewing' && <div style={progressText}>{progress}</div>}
            {previewResult && !error?.section?.startsWith('preview') && (
              <div style={sectionBox}>
                <div style={{ color: '#aaa' }}>
                  {previewResult.local_only} local-only · {previewResult.remote_only} remote-only · {previewResult.shared_path} shared paths
                </div>
                {previewResult.dead_refs_found > 0 && <div style={{ color: '#888', marginTop: 4 }}>{previewResult.dead_refs_found} dead refs found</div>}
              </div>
            )}
            {error && error.section === 'previewing' && (
              <div style={errBox}><div style={{ color: '#f88' }}>{error.message}</div>
                <button type="button" style={btn(false)} onClick={handleRetry}>Retry</button></div>
            )}

            {previewResult && !error?.section?.startsWith('preview') && (
              <div style={sectionBox}>
                {!isSharedHistory && (
                  <div style={{ display: 'flex', alignItems: 'center', gap: 16, marginBottom: 10 }}>
                    <span style={{ color: '#888', fontSize: 12 }}>Conflict strategy:</span>
                    <label style={radio(busy)}><input type="radio" name="strategy" checked={strategy === 'local'} disabled={busy} onChange={() => setStrategy('local')} /> Local wins</label>
                    <label style={radio(busy)}><input type="radio" name="strategy" checked={strategy === 'remote'} disabled={busy} onChange={() => setStrategy('remote')} /> Remote wins</label>
                  </div>
                )}
                {isSharedHistory && <div style={{ color: '#aaa', fontSize: 13 }}>Histories share an ancestor — sync reconciles local and remote on its next cycle. No merge preview needed.</div>}
                {step === 'applying' && <div style={progressText}>{progress}</div>}
                {step === 'applied' && applyResult && !error && !isSharedHistory && (
                  <div style={{ color: '#aaa' }}>
                    <span style={{ color: '#4caf50' }}>Merge preview ready — </span>
                    {applyResult.total_facts} facts: {applyResult.from_local} local, {applyResult.from_remote} remote{applyResult.overwrites > 0 && ` (${applyResult.overwrites} overwrites)`}
                  </div>
                )}
                {error && error.section === 'applying' && (
                  <div><div style={{ color: '#f88' }}>{error.message}</div>
                    <button type="button" style={btn(false)} onClick={handleRetry}>Retry</button></div>
                )}
              </div>
            )}
          </>
        )}

        {/* ── Step ③ Sync ── */}
        {activeStep === 3 && (
          <div style={sectionBox}>
            {step === 'committing' && <div style={progressText}>{progress}</div>}
            {step === 'done' && <div style={{ color: '#4caf50' }}>Remote connected successfully.</div>}
            {error && error.section === 'committing' && (
              <div><div style={{ color: '#f88' }}>{error.message}</div>
                <button type="button" style={btn(false)} onClick={handleRetry}>Retry</button></div>
            )}
          </div>
        )}
      </div>

      {/* ── Footer ── */}
      {activeStep === 1 && (
        <footer style={foot}>
          <button type="button" style={btn(false, 'secondary')} onClick={cancel}>Cancel</button>
          <button type="button" data-testid="wizard-test" style={btn(!canTest)} disabled={!canTest} onClick={handleTest}>Test connection →</button>
        </footer>
      )}
      {activeStep === 2 && (
        <footer style={foot}>
          <button type="button" style={btn(busy, 'secondary')} disabled={busy} onClick={handleBack}>← Back</button>
          <div style={{ display: 'flex', gap: 8 }}>
            {step === 'applied' && !error && !isSharedHistory && (
              <button type="button" style={btn(false, 'secondary')} onClick={() => { setApplyResult(null); setStep('previewed'); }}>Try different strategy</button>
            )}
            {(step === 'previewed' || step === 'applying') && !isSharedHistory && (
              <button type="button" data-testid="wizard-preview" style={btn(busy || !previewResult)} disabled={busy || !previewResult} onClick={handleApply}>Preview merge →</button>
            )}
            {(step === 'previewed' && isSharedHistory) && (
              <button type="button" data-testid="wizard-connect" style={btn(busy)} disabled={busy} onClick={handleApply}>Connect →</button>
            )}
            {step === 'applied' && !error && (
              <button type="button" data-testid="wizard-connect" style={btn(false)} onClick={handleCommit}>Connect →</button>
            )}
          </div>
        </footer>
      )}
    </div>
  );
}

// ── styles ──
const wrap: React.CSSProperties = { display: 'flex', flexDirection: 'column', height: '100%', color: '#eee' };
const head: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '14px 18px', borderBottom: '1px solid #222' };
const iconBtn: React.CSSProperties = { background: 'none', border: 'none', color: '#aaa', fontSize: 16, cursor: 'pointer' };
const steps: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, padding: '14px 18px 0' };
const stepItem: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8 };
const stepDot = (state: 'active' | 'done' | 'todo'): React.CSSProperties => ({
  width: 22, height: 22, borderRadius: '50%', display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  fontSize: 12, fontWeight: 600,
  background: state === 'active' ? '#1d4ed8' : state === 'done' ? '#14532d' : '#222',
  color: state === 'todo' ? '#777' : '#fff', border: state === 'done' ? '1px solid #2e7d32' : '1px solid #333',
});
const stepBar = (done: boolean): React.CSSProperties => ({ width: 36, height: 2, background: done ? '#2e7d32' : '#333', marginLeft: 2 });
const bodyArea: React.CSSProperties = { flex: 1, overflowY: 'auto', padding: 18 };
const foot: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '14px 18px', borderTop: '1px solid #222' };
const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 10, display: 'block' };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#1a1a1a', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const sectionBox: React.CSSProperties = { marginTop: 12, padding: 12, background: '#111', borderRadius: 4, fontSize: 13 };
const errBox: React.CSSProperties = { marginTop: 12, padding: 12, background: '#1a1111', border: '1px solid #533', borderRadius: 4, fontSize: 13 };
const errText: React.CSSProperties = { color: '#f88', fontSize: 12, marginTop: 8 };
const progressText: React.CSSProperties = { fontSize: 13, color: '#8af', marginTop: 8 };
const radio = (busy: boolean): React.CSSProperties => ({ display: 'flex', alignItems: 'center', gap: 4, cursor: busy ? 'not-allowed' : 'pointer', color: '#ccc', fontSize: 13 });
const btn = (disabled: boolean, variant: 'primary' | 'secondary' = 'primary'): React.CSSProperties => ({
  padding: '7px 16px', borderRadius: 4, border: '1px solid #333', fontSize: 13, cursor: disabled ? 'not-allowed' : 'pointer',
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#fff',
});
