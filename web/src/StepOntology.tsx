import { useEffect, useRef, useState } from 'react';
import { api, type OntologyDiagnostic, type OntologyPreset, type OntologyValidation } from './api';
import type { WizardAction, WizardState } from './wizardState';
import { OntologyEditor } from './OntologyEditor';

// Mirrors internal/web/handlers_ontologies.go's MaxOntologyBytes. The server
// enforces this regardless (413 Request Entity Too Large); this client-side
// check exists only so an oversize upload gets a clear message instead of a
// round trip that ends in a generic HTTP error.
const MAX_ONTOLOGY_BYTES = 256 * 1024;

// DEBOUNCE_MS is how long "Write your own" waits after the last keystroke
// before re-validating — mirrors the editor's own lint debounce (see
// OntologyEditor.tsx) so the panel and the in-editor squiggles update in step.
const DEBOUNCE_MS = 400;

type Mode = 'preset' | 'upload' | 'write';

// StepOntology owns the four choices (two presets, upload, write-your-own)
// and the verification panel that renders a parsed summary or diagnostics.
// It does NOT know about CodeMirror internals — that boundary lives in
// OntologyEditor, which this component treats as a plain value/onChange box.
//
// Appears only for seed-an-empty-remote and local-only (stepsFor in
// wizardState.ts) — joining a remote that already has content never reaches
// this step, because that ontology comes from the remote and the backend
// hard-refuses one in 'clone' mode.
export function StepOntology({ state, onDispatch, onValidityChange }: {
  state: WizardState;
  onDispatch: (a: WizardAction) => void;
  onValidityChange: (valid: boolean) => void;
}) {
  const [presets, setPresets] = useState<OntologyPreset[]>([]);
  // Which of the four choices is showing. Derived once at mount from wizard
  // state (yaml already present -> the user was mid-edit/upload last time
  // this step was on screen; otherwise a preset is selected, matching
  // initialWizardState.preset === 'default'). Local UI state, not stored in
  // the wizard reducer: SET_PRESET/SET_YAML only need to know WHAT ontology
  // was chosen, not which of upload/write produced a custom one.
  const [mode, setMode] = useState<Mode>(() => (state.yaml ? 'write' : 'preset'));

  const [uploadResult, setUploadResult] = useState<OntologyValidation | null>(null);
  const [uploadError, setUploadError] = useState('');
  const [uploadBusy, setUploadBusy] = useState(false);

  const [seedBusy, setSeedBusy] = useState(false);
  const [seedError, setSeedError] = useState('');
  const [writeResult, setWriteResult] = useState<OntologyValidation | null>(null);

  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  // Bumped on EVERY mode transition (selectPreset, startUpload, startWriteOwn)
  // and every new file selection — not just same-mode re-selection. A response
  // for an abandoned choice (switched away mid-fetch, or a second file/preset
  // picked before the first round trip resolves) must never land on state for
  // a choice the user no longer has selected: the ontology is immutable after
  // repo creation, so "the user got an ontology they didn't pick" has no
  // repair path short of recreating the repo. One counter shared by all four
  // call sites closes the cross-mode case by construction, rather than three
  // separate refs that each only guard their own mode.
  const opSeq = useRef(0);

  useEffect(() => {
    let cancelled = false;
    api.ontologyPresets().then(p => { if (!cancelled) setPresets(p); }).catch(() => {});
    return () => { cancelled = true; };
  }, []);

  // A preset is server-supplied and already parses — SELECTING one is valid
  // with no validate round trip. Fires on mount too, since
  // initialWizardState.preset === 'default' selects one before any click.
  //
  // Gated on state.preset rather than reporting a flat `true`, because preset
  // mode is also where an EMPTIED editor lands: SET_YAML('') leaves
  // {yaml:'', preset:''}, the mount initialiser above then reads no yaml and
  // picks 'preset', and no card renders as selected. Reporting valid there
  // enabled Next over a selection the user cannot see, and createBodyFor's
  // `state.preset || 'default'` turned it into the default ontology —
  // immutable after creation, so unfixable short of recreating the repo.
  useEffect(() => {
    if (mode === 'preset') onValidityChange(state.preset !== '');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, state.preset]);

  function selectPreset(p: OntologyPreset) {
    opSeq.current++; // abandons any in-flight upload validate or seed fetch
    setMode('preset');
    setUploadError(''); setUploadResult(null);
    setSeedBusy(false); setSeedError(''); setWriteResult(null);
    onDispatch({ type: 'SET_PRESET', preset: p.name });
  }

  function startUpload() {
    // Clicking the card you are already on is a no-op, never a reset. This
    // panel's own state (the chosen file's validation summary, and the Next
    // gate derived from it) is the only thing the reset below would throw
    // away, and the user asked for nothing by clicking a card that is already
    // selected. Same rule as startWriteOwn's guard below, for the same reason.
    if (mode === 'upload') return;
    opSeq.current++; // abandons any in-flight seed fetch
    setMode('upload');
    setSeedBusy(false); setSeedError(''); setWriteResult(null);
    // No file chosen yet — without this, Next would stay enabled off
    // whatever preset was selected before switching panels, even though the
    // visible panel now promises "pick a file", not "use that preset".
    onValidityChange(false);
  }

  // startWriteOwn SEEDS ONLY WHEN THERE IS NOTHING TO LOSE.
  //
  // The card has no disabled guard, so it is clickable while already selected —
  // and an unconditional seed there fetched a preset and dispatched SET_YAML
  // straight over whatever the user had typed. Worse, `state.preset` is ''
  // by then (SET_YAML cleared it), so the refetch resolved to 'default': a
  // user who picked "code", opened the editor and edited it lost the edits AND
  // got the default ontology, permanently, because the ontology is immutable
  // after repo creation.
  //
  // So: any existing content — an earlier seed the user has since edited, or a
  // file they just uploaded — is carried into the editor untouched, and the
  // write-mode validate effect below re-validates it. Only an EMPTY editor
  // gets a seed, and that seed comes from state.seedPreset (the card the user
  // actually clicked, remembered in the reducer precisely so it survives both
  // SET_YAML and a remount of this step), never from a fallback.
  async function startWriteOwn() {
    const seq = ++opSeq.current; // abandons any in-flight upload validate
    const stale = () => !mounted.current || seq !== opSeq.current;
    setMode('write');
    setUploadError(''); setUploadResult(null);
    setSeedError('');
    onValidityChange(false); // present but not yet validated
    if (state.yaml !== '') {
      // Also clears a seedBusy left set by a fetch that was abandoned in
      // another mode (its own finally is suppressed by the stale check), which
      // would otherwise pin this panel on "Loading a starting point…" and never
      // render the editor holding the content we just refused to overwrite.
      setSeedBusy(false);
      return;
    }
    setSeedBusy(true);
    try {
      const seed = await api.ontologyPresetYAML(state.seedPreset);
      if (stale()) return;
      onDispatch({ type: 'SET_YAML', yaml: seed });
    } catch (e) {
      if (stale()) return;
      setSeedError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!stale()) setSeedBusy(false);
    }
  }

  async function handleFile(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    e.target.value = ''; // allow re-selecting the same file after fixing it
    if (!file) return;
    const seq = ++opSeq.current; // abandons any earlier upload or seed fetch
    const stale = () => !mounted.current || seq !== opSeq.current;
    setUploadResult(null);
    setUploadError('');
    if (file.size > MAX_ONTOLOGY_BYTES) {
      setUploadError(`That file is ${Math.ceil(file.size / 1024)} KiB — ontologies are capped at 256 KiB.`);
      onValidityChange(false);
      return;
    }
    const text = await file.text();
    if (stale()) return;
    onDispatch({ type: 'SET_YAML', yaml: text });
    onValidityChange(false); // present but not yet validated
    setUploadBusy(true);
    try {
      const result = await api.validateOntology(text);
      if (stale()) return;
      setUploadResult(result);
      onValidityChange(result.ok);
    } catch (err) {
      if (stale()) return;
      setUploadError(err instanceof Error ? err.message : String(err));
      onValidityChange(false);
    } finally {
      if (!stale()) setUploadBusy(false);
    }
  }

  function handleEditorChange(text: string) {
    onDispatch({ type: 'SET_YAML', yaml: text });
  }

  // Re-validate "write your own" content DEBOUNCE_MS after the user stops
  // typing, driving this panel's summary/diagnostics and the Next gate. This
  // is independent of the editor's own inline lint (OntologyEditor.tsx) —
  // that call renders squiggles in the editor; this one renders the panel
  // below it and decides whether Next may be clicked. Skipped entirely while
  // the seed fetch is still in flight, so a stale empty doc never validates.
  useEffect(() => {
    if (mode !== 'write' || seedBusy) return;
    let cancelled = false;
    if (!state.yaml.trim()) {
      setWriteResult(null);
      onValidityChange(false);
      return;
    }
    onValidityChange(false);
    const timer = setTimeout(async () => {
      try {
        const result = await api.validateOntology(state.yaml);
        if (cancelled) return;
        setWriteResult(result);
        onValidityChange(result.ok);
      } catch {
        if (cancelled) return;
        setWriteResult(null);
        onValidityChange(false);
      }
    }, DEBOUNCE_MS);
    return () => { cancelled = true; clearTimeout(timer); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, state.yaml, seedBusy]);

  return (
    <div data-testid="step-ontology">
      <label style={label}>Ontology</label>
      <p style={hint}>
        The ontology is the topic tree and validation rules new facts are checked
        against. Start from a preset, upload one, or write your own.{' '}
        {/* target="_blank": the desktop build is a WKWebView, so an in-frame
            navigation here would strand the reader with no way back. */}
        <a href="https://knomit.io/docs/ontology" target="_blank" rel="noreferrer" style={link}>
          What is an ontology?
        </a>
      </p>

      <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', marginTop: 8 }}>
        {presets.map(p => (
          <button key={p.name} type="button" style={presetCard(mode === 'preset' && state.preset === p.name)}
            onClick={() => selectPreset(p)}>
            <div style={presetTitle}>{p.title}</div>
            <div style={presetBody}>{p.description}</div>
            <div style={presetTopics}>{p.topics.join(', ')}</div>
          </button>
        ))}
        <button type="button" style={presetCard(mode === 'upload')} onClick={startUpload}>
          <div style={presetTitle}>Upload a file</div>
          <div style={presetBody}>Bring an ontology YAML from elsewhere.</div>
        </button>
        <button type="button" style={presetCard(mode === 'write')} onClick={startWriteOwn}>
          <div style={presetTitle}>Write your own</div>
          <div style={presetBody}>Start from the selected preset and edit it.</div>
        </button>
      </div>

      {/* Preset mode with nothing selected is reachable exactly one way: the
          user emptied the editor and came back to this step, so state is
          {yaml:'', preset:''}. Next is correctly disabled there — but a
          disabled button next to four unselected cards says nothing about why,
          so say it. Plain text, not amber: nothing has failed. */}
      {mode === 'preset' && state.preset === '' && (
        <div style={hint}>Pick one of the choices above to continue.</div>
      )}

      {mode === 'upload' && (
        <div style={panel}>
          <label style={label}>Ontology file</label>
          <input data-testid="ontology-file" type="file" accept=".yaml,.yml,text/yaml" onChange={handleFile} />
          {uploadBusy && <div style={hint}>Validating…</div>}
          {!uploadBusy && uploadError && <div style={warnText}>{uploadError}</div>}
          {!uploadBusy && uploadResult && <ValidationSummary result={uploadResult} />}
        </div>
      )}

      {mode === 'write' && (
        <div style={panel}>
          {seedBusy && <div style={hint}>Loading a starting point…</div>}
          {seedError && <div style={warnText}>{seedError}</div>}
          {!seedBusy && !seedError && (
            <>
              <OntologyEditor value={state.yaml} onChange={handleEditorChange} />
              {writeResult && <ValidationSummary result={writeResult} />}
            </>
          )}
        </div>
      )}
    </div>
  );
}

// ValidationSummary renders an OntologyValidation result: the parsed
// id/name/topic-count/rule-count on success, or each diagnostic as
// "line N — message" on failure. Amber is reserved for genuine failure —
// diagnostics qualify; the success summary stays plain text.
function ValidationSummary({ result }: { result: OntologyValidation }) {
  if (result.ok) {
    return (
      <div style={summary}>
        <div>{result.id} — {result.name}</div>
        <div style={summaryDetail}>{result.topics.length} topics</div>
        <div style={summaryDetail}>{result.rule_count} validation rules</div>
      </div>
    );
  }
  return (
    <div style={summary}>
      {result.diagnostics.map((d: OntologyDiagnostic, i: number) => (
        <div key={i} style={warnText}>Line {d.line} — {d.message}</div>
      ))}
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5 };
const link: React.CSSProperties = { color: '#6ea8fe' };
const panel: React.CSSProperties = { marginTop: 12, padding: '10px 12px', background: '#111', border: '1px solid #2a2a2a', borderRadius: 6 };
const presetCard = (selected: boolean): React.CSSProperties => ({
  flex: '1 1 200px', textAlign: 'left', cursor: 'pointer',
  padding: '10px 12px', borderRadius: 6,
  background: selected ? '#1a2a1a' : '#111',
  border: '1px solid ' + (selected ? '#2a4a2a' : '#2a2a2a'),
  color: '#eee',
});
const presetTitle: React.CSSProperties = { fontSize: 14, fontWeight: 600 };
const presetBody: React.CSSProperties = { fontSize: 12, color: '#999', margin: '4px 0' };
const presetTopics: React.CSSProperties = { fontSize: 11, color: '#666' };
const summary: React.CSSProperties = { marginTop: 8, fontSize: 12, color: '#ccc', lineHeight: 1.6 };
const summaryDetail: React.CSSProperties = { color: '#999' };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 4 };
