import { useState, useRef, useCallback, useEffect } from 'react';
import { api, createSession, streamTest, streamPreview, streamApply, streamCommit, deleteSession } from './api';
import type { SSEEvent, TestResult, PreviewResult, ApplyResult } from './api';
import { GlobeIcon } from './icons';
import { btn, card } from './manageStyles';
import { repoHue, repoHueBg, repoHueBorder, noMouseFocus } from './utils';

type Step =
  | 'idle' | 'creating' | 'testing' | 'tested'
  | 'previewing' | 'previewed' | 'applying' | 'applied'
  | 'committing' | 'done';

interface Props {
  repo: string;
  onCancel: () => void;   // user backed out — return to the repo's settings
  onDone: () => void;     // remote connected — return to the repo's settings
  // True while the commit is in flight (and through its brief success window).
  // The parent MUST NOT unmount this component while it is true — see the
  // `leavable` comment below for what unmounting costs.
  onBusyChange?: (busy: boolean) => void;
}

// RemoteConnectWizard is the stepped connect/reconcile flow. It drives the
// session backend (test → preview → apply → commit) and presents three steps:
//   ① Connect   ② Review   ③ Sync
//
// It is a SUB-PAGE of the repo it configures, not a surface of its own: the
// manager's rail stays, and this renders in the detail column under a crumb
// back to the repo. It used to replace the whole Manage surface with a 720px
// column stretched to the window's full height, which left two form fields
// floating above 600px of nothing with the actions pinned to the bottom edge —
// dressed as a dialog (title bar, ✕, Cancel) with no dialog around it.
//
// So it borrows SettingsPage's grammar rather than inventing a third one: the
// same column-plus-rail grid, the same block headings, and a rail in the slot
// "On this page" occupies on the settings page you just came from. That rail is
// the progress tracker — the only structural difference, and it earns the
// difference because progress is genuinely what a reader wants there mid-flow.
export function RemoteConnectWizard({ repo, onCancel, onDone, onBusyChange }: Props) {
  const [url, setUrl] = useState('');
  // '' = auto-detect (backend infers SSH for git@/ssh:// URLs, else anonymous).
  // 'none' forces anonymous even for SSH-style URLs. handleTest sends '' as an
  // omitted auth_method so the backend's auto-promotion can run.
  const [authMethod, setAuthMethod] = useState<'' | 'none' | 'ssh' | 'token' | 'basic'>('');
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
  // The success pause before handing back to the parent. It has to be cancelled
  // on unmount: it fires onDone(), and onDone MOVES THE SELECTION. Left running,
  // a reader who navigates away during the 1.2s window gets yanked back to this
  // repo's settings page a beat later, from wherever they went.
  const doneTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Set once the reader has left — by the crumb, or by the page being unmounted
  // out from under a running step. handleApply reads it before chaining into
  // the commit, which is the one hand-off that would otherwise outlive the exit:
  // the crumb stays enabled during 'applying' (nothing is written yet, so
  // leaving is free), but the shared-history path chains apply → commit, and a
  // commit is a store swap. Without this the reader backs out and the swap runs
  // anyway, on a session cancel() has already asked the server to delete.
  const leftRef = useRef(false);
  useEffect(() => () => {
    leftRef.current = true;
    cleanupRef.current?.();
    if (doneTimerRef.current !== null) clearTimeout(doneTimerRef.current);
  }, []);

  // Prefill from the existing origin (reconnect/change case). The secret is
  // never returned by the API, so token/password start blank.
  useEffect(() => {
    let cancelled = false;
    api.getOrigin(repo).then(o => {
      if (cancelled || !o) return;
      if (o.url) setUrl(o.url);
      const m = o.auth_method;
      if (m === 'none' || m === 'ssh' || m === 'token' || m === 'basic') setAuthMethod(m);
    }).catch(() => { /* leave blank */ });
    return () => { cancelled = true; };
  }, [repo]);

  const cancel = useCallback(() => {
    leftRef.current = true;
    cleanupRef.current?.(); cleanupRef.current = null;
    if (sessionId) deleteSession(repo, sessionId).catch(() => {});
    onCancel();
  }, [repo, sessionId, onCancel]);

  const handleTest = async () => {
    setError(null); setStep('creating'); setProgress('Creating session…');
    // Whether the session came up, tracked locally. The catch below used to ask
    // `step === 'creating'` instead — the value captured at click time, which is
    // always 'idle' — so the reset never ran: a rejected createSession (bad URL,
    // server down) left the page at 'creating' forever, every control disabled
    // by `busy`, and the error box is gated on 'idle' so the reason never
    // showed either. The only way out was to leave and start over.
    let created = false;
    try {
      const sess = await createSession(repo, {
        url,
        auth_method: authMethod || undefined,
        token: authMethod === 'token' ? token : undefined,
        user: authMethod === 'basic' ? user : undefined,
        password: authMethod === 'basic' ? password : undefined,
      });
      created = true;
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
      // Once the session exists, the only thing that rejects the promise above
      // is the stream's own 'error' event — which has already recorded its
      // message and returned the page to 'idle'. Overwriting it here would
      // replace "auth failed" with a generic line.
      if (created) return;
      setError({ section: 'creating', message: (e instanceof Error && e.message) || 'Failed to create session' });
      setStep('idle'); setProgress('');
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

  const handleCommit = async () => {
    if (!sessionId) return;
    setError(null); setStep('committing'); setProgress('Finalizing…');
    try {
      await streamCommit(repo, sessionId, (ev: SSEEvent) => {
        if (ev.phase === 'done') {
          setStep('done'); setProgress('');
          doneTimerRef.current = setTimeout(() => onDone(), 1200);
        } else if (ev.phase === 'error') {
          // Keep step at 'committing' — it is what keeps the Sync block, and so
          // this error and its Retry, on screen. `leavable` reads the error, not
          // the step, to know the commit is no longer in flight.
          setError({ section: 'committing', message: ev.message }); setProgress('');
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
      setProgress('');
    }
  };

  // handleApply runs the merge. When thenCommit is set (the shared-history
  // single "Connect" path), it chains straight into commit so the user clicks
  // once instead of twice. For non-shared history it stops at the merge preview
  // so the user can review counts / try a different strategy before committing.
  const handleApply = async (thenCommit = false) => {
    if (!sessionId) return;
    setError(null); setApplyResult(null); setStep('applying'); setProgress('Merging…');
    let ok = true;
    try {
      await streamApply(repo, sessionId, strategy === 'local' ? 'local_wins' : 'remote_wins', selectedBranch || undefined, (ev: SSEEvent) => {
        if (ev.phase === 'done') {
          setApplyResult(ev.result as ApplyResult); setStep('applied'); setProgress('');
        } else if (ev.phase === 'error') {
          ok = false; setError({ section: 'applying', message: ev.message }); setStep('applied');
        } else if (ev.phase === 'replaying') {
          // current/total are only present once per-fact progress starts; the
          // initial "replaying" event (and remote-only replays, which have no
          // per-fact loop) omit them — don't render "undefined/undefined".
          setProgress(ev.total ? `Replaying ${ev.current}/${ev.total}…` : 'Replaying…');
        } else if (ev.phase === 'merging') {
          setProgress('Merging…');
        } else { setProgress(ev.phase + '…'); }
      });
    } catch (e) {
      ok = false; setError({ section: 'applying', message: (e instanceof Error && e.message) || 'Apply failed' }); setStep('applied');
    }
    // `leftRef` and not a state flag: this runs after an await, so a state read
    // here would be the value captured when the click happened. Leaving during
    // the merge is allowed — nothing is written yet — but it must not hand off
    // to the step that writes.
    if (ok && thenCommit && !leftRef.current) await handleCommit();
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
  // Non-blocking advisory: 'none' on an SSH-style URL is a deliberate override
  // (force anonymous), but it almost always fails to authenticate. Warn without
  // disabling Test so the override stays usable for the rare anonymous host.
  const authWarning = (isSSHURL && authMethod === 'none')
    ? 'Anonymous auth on an SSH URL usually fails — choose SSH (knomit key) unless this host allows anonymous access.'
    : '';
  const canTest = !!url && !authMismatch && step === 'idle';
  const busy = step === 'creating' || step === 'testing' || step === 'previewing' || step === 'applying' || step === 'committing';
  const isSharedHistory = testResult?.history === 'shared';

  const onReview = testResult && step !== 'idle' && step !== 'creating' && step !== 'testing'
    && step !== 'committing' && step !== 'done';
  const activeStep: 1 | 2 | 3 = (step === 'committing' || step === 'done') ? 3 : onReview ? 2 : 1;

  // Leaving is withheld once the commit is in flight: it swaps the store and
  // rebuilds the index, and there is no "un-commit" to return the reader to.
  //
  // A FAILED commit is not in flight — the stream is over and the only moves
  // left are Retry or leaving — but its step stays 'committing' so the Sync
  // block keeps rendering the error. So "still running" is step AND no error;
  // reading the step alone would strand the reader on a page whose only exit is
  // disabled, under a rail note telling them to wait for work that already died.
  const commitFailed = step === 'committing' && error?.section === 'committing';
  const leavable = (step !== 'committing' || commitFailed) && step !== 'done';

  // Published upward because the crumb is not the only exit any more: this page
  // renders inside the manager, whose rail can unmount it out from under a live
  // commit. streamCommit has no AbortController and is not registered in
  // cleanupRef, so unmounting does not stop it — it strands a store swap whose
  // completion the UI never hears, and leaves its temp dir open to being deleted
  // by the next session the user starts.
  useEffect(() => {
    onBusyChange?.(!leavable);
    return () => { onBusyChange?.(false); };
  }, [leavable, onBusyChange]);

  return (
    <div data-testid="remote-connect-wizard">
      {/* The crumb is the way back, and the only one. It also states where you
          are — a sub-page of a repository, not a mode — which is the thing the
          old full-surface takeover could not say. */}
      <nav style={crumb} aria-label="Breadcrumb">
        <button type="button" data-testid="wizard-crumb-back" className="k-bare"
          onMouseDown={noMouseFocus} style={crumbLink(!leavable)} disabled={!leavable} onClick={cancel}>
          {repo}
        </button>
        <span style={crumbSep} aria-hidden="true">/</span>
        <span>Connect remote</span>
      </nav>

      <div style={pageHead}>
        <span style={iconBox(repo)}><GlobeIcon color={repoHue(repo)} size={16} /></span>
        <div style={{ minWidth: 0 }}>
          <h3 style={{ margin: 0, fontSize: 16 }}>Connect remote</h3>
          <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>pull and push {repo} against an origin</div>
        </div>
      </div>

      <div style={grid}>
        <div style={column}>
          {/* ── Step ① Connect ── */}
          {activeStep === 1 && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Origin</h4>
                <span style={blockHint}>the git remote this repository syncs against</span>
              </div>
              <div>
                <label style={label} htmlFor="wizard-url-input">Remote URL</label>
                <input id="wizard-url-input" data-testid="wizard-url" style={input} value={url} disabled={busy}
                  placeholder="https://… · git@host:repo · /path/to/repo"
                  onChange={e => setUrl(e.target.value)} />
                <label style={label} htmlFor="wizard-auth-select">Auth method</label>
                <select id="wizard-auth-select" style={input} value={authMethod} disabled={busy}
                  onChange={e => setAuthMethod(e.target.value as typeof authMethod)}>
                  <option value="">Auto-detect</option>
                  <option value="none">None (anonymous)</option>
                  <option value="ssh">SSH (knomit key)</option>
                  <option value="token">Token</option>
                  <option value="basic">Basic (user / password)</option>
                </select>
                {authMethod === 'token' && (<>
                  <label style={label} htmlFor="wizard-token">Token</label>
                  <input id="wizard-token" style={input} type="password" value={token} disabled={busy} placeholder="ghp_…" onChange={e => setToken(e.target.value)} />
                </>)}
                {authMethod === 'basic' && (<>
                  <label style={label} htmlFor="wizard-user">Username</label>
                  <input id="wizard-user" style={input} value={user} disabled={busy} onChange={e => setUser(e.target.value)} />
                  <label style={label} htmlFor="wizard-password">Password</label>
                  <input id="wizard-password" style={input} type="password" value={password} disabled={busy} onChange={e => setPassword(e.target.value)} />
                </>)}
                {authMismatch && <div style={errText}>{authMismatch}</div>}
                {!authMismatch && authWarning && <div data-testid="wizard-auth-warning" style={warnText}>{authWarning}</div>}
                {(step === 'creating' || step === 'testing') && <div style={progressText}>{progress}</div>}
                {error && (step === 'idle') && (
                  <div style={errBox}><div style={{ color: '#f88' }}>{error.message}</div>
                    <button type="button" style={{ ...btn(false), marginTop: 8 }} onClick={handleRetry}>Retry</button></div>
                )}
              </div>
              <div style={actions}>
                <button type="button" data-testid="wizard-test" style={btn(!canTest, 'primary')} disabled={!canTest} onClick={handleTest}>Test connection →</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={cancel}>Cancel</button>
              </div>
            </section>
          )}

          {/* ── Step ② Review ── */}
          {activeStep === 2 && testResult && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Review</h4>
                <span style={blockHint}>what the two sides hold, and which of them wins</span>
              </div>
              <div style={{ fontSize: 12, color: '#888', wordBreak: 'break-all' }}>
                {url} — {testResult.remote_fact_count} remote · {testResult.local_fact_count} local · {testResult.history} histories
              </div>
              <div style={{ display: 'flex', gap: 12, fontSize: 13, alignItems: 'center', flexWrap: 'wrap' }}>
                <span style={{ color: '#888' }}>Branch:</span>
                <select value={selectedBranch} disabled={busy || (testResult.branches ?? []).length === 0} onChange={e => setSelectedBranch(e.target.value)}
                  aria-label="Remote branch"
                  style={{ background: '#0c0c0c', border: '1px solid #333', color: '#eee', padding: '4px 7px', borderRadius: 4, fontSize: 13 }}>
                  {(testResult.branches ?? []).length === 0
                    ? <option value="">(no remote branches yet)</option>
                    : (testResult.branches ?? []).map(b => <option key={b} value={b}>{b}</option>)}
                </select>
                {testResult.matched_agent && <span style={{ color: '#8af', fontSize: 12 }}>agent branch found — will replay on top</span>}
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
                  <button type="button" style={{ ...btn(false), marginTop: 8 }} onClick={handleRetry}>Retry</button></div>
              )}

              {previewResult && !error?.section?.startsWith('preview') && (
                <div style={sectionBox}>
                  {!isSharedHistory && (
                    <div style={{ display: 'flex', alignItems: 'center', gap: 16, marginBottom: 10, flexWrap: 'wrap' }}>
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
                      <button type="button" style={{ ...btn(false), marginTop: 8 }} onClick={handleRetry}>Retry</button></div>
                  )}
                </div>
              )}

              <div style={actions}>
                {(step === 'previewed' || step === 'applying') && !isSharedHistory && (
                  <button type="button" data-testid="wizard-preview" style={btn(busy || !previewResult, 'primary')} disabled={busy || !previewResult} onClick={() => handleApply(false)}>Preview merge →</button>
                )}
                {step === 'previewed' && isSharedHistory && (
                  <button type="button" data-testid="wizard-connect" style={btn(busy, 'primary')} disabled={busy} onClick={() => handleApply(true)}>Connect →</button>
                )}
                {/* Shared history is included, and that is the whole exit from a
                    failed commit: 'applied' is where Retry lands, and with the
                    button excluded here the shared path arrived at a step with
                    no Connect, no Preview and no Retry — only "← Back", which
                    throws the session away. The shared flow reaches 'applied'
                    only for an instant on its way into the commit it chains, so
                    in practice this button belongs to the retry. */}
                {step === 'applied' && !error && (
                  <button type="button" data-testid="wizard-connect" style={btn(false, 'primary')} onClick={handleCommit}>Connect →</button>
                )}
                {step === 'applied' && !error && !isSharedHistory && (
                  <button type="button" style={btn(false)} onClick={() => { setApplyResult(null); setStep('previewed'); }}>Try different strategy</button>
                )}
                <button type="button" style={btn(busy)} disabled={busy} onClick={handleBack}>← Back</button>
              </div>
            </section>
          )}

          {/* ── Step ③ Sync ── */}
          {activeStep === 3 && (
            <section style={block}>
              <div style={blockHead}>
                <h4 style={blockTitle}>Sync</h4>
                <span style={blockHint}>swapping the store and rebuilding the index</span>
              </div>
              <div style={sectionBox}>
                {step === 'committing' && <div style={progressText}>{progress}</div>}
                {step === 'done' && <div style={{ color: '#4caf50' }}>Remote connected successfully.</div>}
                {error && error.section === 'committing' && (
                  <div><div style={{ color: '#f88' }}>{error.message}</div>
                    <button type="button" style={{ ...btn(false), marginTop: 8 }} onClick={handleRetry}>Retry</button></div>
                )}
              </div>
            </section>
          )}
        </div>

        {/* The rail sits where SettingsPage's contents rail sits, so crossing
            from the repo's settings into this page does not move it. It is a
            tracker, not an index: the steps are not reachable out of order, so
            they are text rather than buttons — an inert-looking row is the
            honest rendering of a place you cannot click to. */}
        <nav style={rail} aria-label="Progress">
          <div style={railLabel}>Progress</div>
          {([[1, 'Connect'], [2, 'Review'], [3, 'Sync']] as const).map(([n, lbl]) => {
            const state = step === 'done' ? 'done' : activeStep === n ? 'active' : activeStep > n ? 'done' : 'todo';
            return (
              <div key={n} data-testid={`wizard-step-${n}`} aria-current={state === 'active' ? 'step' : undefined} style={railItem(state)}>
                <span style={railMark(state)}>{state === 'done' ? '✓' : n}</span>
                <span>{lbl}</span>
              </div>
            );
          })}
          <p style={railNote}>
            {/* Deliberately does NOT claim the repo is unchanged: it usually is
                (every refusal aborts before the swap and says so in its own
                message), but a failed swap — or a stream that dropped after the
                server got past it — cannot promise that from here. */}
            {commitFailed
              ? <>The last step did not finish. Retry, or go back to <b style={{ color: '#8a8a8a', fontWeight: 600 }}>{repo}</b>.</>
              : leavable
                ? <>Nothing is written to <b style={{ color: '#8a8a8a', fontWeight: 600 }}>{repo}</b> until the last step runs.</>
                : <>Writing to <b style={{ color: '#8a8a8a', fontWeight: 600 }}>{repo}</b> — leave this running.</>}
          </p>
        </nav>
      </div>
    </div>
  );
}

// ── styles ──
//
// Deliberately NOT a private button/input/card set: this page used to carry its
// own btn(), input and sectionBox, which is why its controls did not match the
// settings page one click away. `btn` and `card` come from manageStyles; what is
// left here is the page's own structure, and the block/rail atoms mirror
// SettingsPage's so the two pages read as one grammar.

const crumb: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 7, fontSize: 12, color: '#888', marginBottom: 10,
};
const crumbLink = (disabled: boolean): React.CSSProperties => ({
  background: 'none', border: 'none', padding: 0, fontSize: 12,
  color: disabled ? '#555' : '#6ea8fe', cursor: disabled ? 'default' : 'pointer',
});
const crumbSep: React.CSSProperties = { color: '#444' };

const pageHead: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 };
// Tinted in the repo's own hue rather than a generic accent — this page belongs
// to that repository, and the icon box is where every other detail head says so.
const iconBox = (repo: string): React.CSSProperties => ({
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: repoHueBg(repo), border: '1px solid ' + repoHueBorder(repo),
  display: 'flex', alignItems: 'center', justifyContent: 'center',
});

// Same two-column split as SettingsPage, to the pixel: the rail must not move
// when you cross from the repo's settings into this page.
const grid: React.CSSProperties = {
  display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) 178px', gap: 26, alignItems: 'start',
};
// A form reads worse the wider it gets, so the column is measured inside its
// grid cell rather than filling it.
const column: React.CSSProperties = { maxWidth: 470 };

const block: React.CSSProperties = {
  borderTop: '1px solid #202020', paddingTop: 16, marginTop: 16,
  display: 'flex', flexDirection: 'column', gap: 10,
};
const blockHead: React.CSSProperties = { display: 'flex', alignItems: 'baseline', gap: 10, flexWrap: 'wrap' };
const blockTitle: React.CSSProperties = { margin: 0, fontSize: 13.5, fontWeight: 650, color: '#e6e6e6' };
const blockHint: React.CSSProperties = { fontSize: 11.5, color: '#6e6e6e' };

// Actions sit under the content they act on. The old footer was pinned to the
// window's bottom edge, which put "Test connection" 600px below the field it
// tests.
const actions: React.CSSProperties = { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginTop: 4 };

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 10, display: 'block' };
const input: React.CSSProperties = {
  width: '100%', boxSizing: 'border-box', background: '#0c0c0c', border: '1px solid #333',
  color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13,
};
const sectionBox: React.CSSProperties = { ...card, marginTop: 0, fontSize: 13 };
const errBox: React.CSSProperties = { ...card, marginTop: 0, background: '#1a1111', borderColor: '#533', fontSize: 13 };
const errText: React.CSSProperties = { color: '#f88', fontSize: 12, marginTop: 8 };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 8 };
const progressText: React.CSSProperties = { fontSize: 13, color: '#8af', marginTop: 8 };
const radio = (busy: boolean): React.CSSProperties => ({ display: 'flex', alignItems: 'center', gap: 4, cursor: busy ? 'not-allowed' : 'pointer', color: '#ccc', fontSize: 13 });

// ── progress rail ──
// Sticky, same slot and same type sizes as SettingsPage's contents rail. It
// keeps that rail's single green accent rather than importing a second one:
// green already means "here" in this column, and a step tracker that introduced
// blue would be saying the same thing in a new colour.
const rail: React.CSSProperties = {
  position: 'sticky', top: 0, display: 'flex', flexDirection: 'column', gap: 2, marginTop: 16,
};
const railLabel: React.CSSProperties = {
  fontSize: 9.5, letterSpacing: '0.13em', textTransform: 'uppercase', color: '#4e4e4e', padding: '0 8px 6px',
};
type StepState = 'active' | 'done' | 'todo';
const railItem = (state: StepState): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 8, width: '100%',
  padding: '4px 9px', borderRadius: 4, fontSize: 11.5, textAlign: 'left',
  background: state === 'active' ? '#1a231d' : 'transparent',
  color: state === 'active' ? '#dfe9e2' : state === 'done' ? '#7f9a89' : '#5e5e5e',
  boxShadow: state === 'active' ? 'inset 2px 0 0 -0.5px #7c9' : 'none',
});
// A 15px disc so the numerals sit on the rail's own type scale — the old 22px
// dots were dialog furniture and would tower over an 11.5px label.
const railMark = (state: StepState): React.CSSProperties => ({
  width: 15, height: 15, borderRadius: '50%', flexShrink: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  fontSize: 9, fontWeight: 700,
  background: state === 'active' ? '#24352b' : state === 'done' ? '#1c2a20' : '#1c1c1c',
  border: '1px solid ' + (state === 'active' ? '#3e5c4a' : state === 'done' ? '#2e4636' : '#2c2c2c'),
  color: state === 'todo' ? '#666' : state === 'done' ? '#7c9' : '#dfe9e2',
});
// The safety line the flow never stated. It is a whole-page fact rather than a
// per-step one, so it lives under the tracker instead of beside a button.
const railNote: React.CSSProperties = {
  margin: '12px 9px 0', paddingTop: 10, borderTop: '1px solid #242424',
  fontSize: 10.5, color: '#5e5e5e', lineHeight: 1.5,
};
