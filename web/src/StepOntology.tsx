import { useEffect, useRef, useState } from 'react';
import { api, type OntologyDiagnostic, type OntologyPreset, type OntologyValidation } from './api';
import type { WizardAction, WizardState } from './wizardState';
import { OntologyEditor } from './OntologyEditor';

// Mirrors internal/web/handlers_ontologies.go's MaxOntologyBytes. The server
// enforces this regardless (413 Request Entity Too Large); this client-side
// check exists only so an oversize upload gets a clear message instead of a
// round trip that ends in a generic HTTP error.
const MAX_ONTOLOGY_BYTES = 256 * 1024;

// DEBOUNCE_MS is how long the step waits after the last change before
// re-validating — mirrors the editor's own lint debounce (see
// OntologyEditor.tsx) so the panel and the in-editor squiggles update in step.
const DEBOUNCE_MS = 400;

// Sentinels for the two source options that are not presets. Prefixed so they
// cannot collide with a preset the server adds later.
const UPLOAD = '__upload__';
const BLANK = '__blank__';

// ── One field, one document ──────────────────────────────────────────────
//
// This step asks ONE question: where does this repository's ontology come
// from? Everything else follows from the answer.
//
// It has been through three shapes. Four peer cards (two presets, "Upload a
// file", "Write your own") said uploading was a sibling of picking "Code" — it
// is not, it is a way to START a document. Splitting into Predefined and
// Custom modes fixed that and introduced a worse problem: one document living
// in two places, which then needed a replace dropdown, a confirmation banner,
// and a header line that could describe a document it no longer held.
//
// "Predefined" was never a KIND of ontology. It is a starting point. Every
// path here ends with exactly one YAML document, and the only real questions
// are where it came from and whether it has been changed — so there is one
// source field, one editor, and one line that answers both.
//
// ── doc is not state.yaml ────────────────────────────────────────────────
//
// An untouched preset sends NO yaml: state.yaml stays empty and createBodyFor
// emits mode 'preset', so the server uses its own copy rather than one
// round-tripped through the browser. `doc` (what the editor shows) therefore
// differs from state.yaml until the reader edits, uploads, or starts blank.
// That split is what lets a preset be valid with no validate round trip.
export function StepOntology({ state, onDispatch, onValidityChange }: {
  state: WizardState;
  onDispatch: (a: WizardAction) => void;
  onValidityChange: (valid: boolean) => void;
}) {
  const [presets, setPresets] = useState<OntologyPreset[]>([]);

  // What the editor shows. See the header — not state.yaml until the document
  // stops being a pristine preset.
  const [doc, setDoc] = useState(state.yaml);
  // Where `doc` came from, and whether the reader has had a hand in it.
  //
  // `edited` is tracked rather than derived by comparing against a pristine
  // copy: "the reader typed something" is precisely the fact the wire format
  // turns on, and string equality would call an edit-and-undo a pristine
  // preset while the wizard had already switched to mode 'custom'.
  //
  // Re-entering the step (Back, then Next) remounts this component, so both
  // initialise from the wizard's own state: a non-empty state.yaml can only
  // have come from an edit or an upload, and seedPreset remembers which preset
  // it started from. The filename does not survive that round trip; naming the
  // preset is the honest remainder.
  const [source, setSource] = useState<Source>(
    () => ({ kind: 'preset', name: state.preset || state.seedPreset || 'default' }));
  const [edited, setEdited] = useState(state.yaml !== '');

  const [validation, setValidation] = useState<{ yaml: string; result: OntologyValidation } | null>(null);
  const [seedBusy, setSeedBusy] = useState(false);
  const [seedError, setSeedError] = useState('');
  const [fileError, setFileError] = useState('');
  // A source change asked for while the reader holds unsaved edits — the one
  // action here that destroys work, so the one that asks first. It asks in the
  // field's own line rather than a banner, which used to shove the editor down
  // the page the moment you touched the control.
  const [pending, setPending] = useState('');

  const fileInput = useRef<HTMLInputElement>(null);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  // Bumped on every source change. A seed fetch that resolves after the reader
  // has moved on must never land: the ontology is immutable once the repo is
  // created, so "you got an ontology you did not pick" has no repair path
  // short of deleting the repo.
  const opSeq = useRef(0);

  useEffect(() => {
    let cancelled = false;
    api.ontologyPresets().then(p => { if (!cancelled) setPresets(p); }).catch(() => {});
    return () => { cancelled = true; };
  }, []);

  // Show the starting preset on arrival. Guarded on an empty state.yaml so
  // returning to the step never overwrites work — `doc` does not survive the
  // unmount, but the wizard's state.yaml does, and the initialiser above has
  // already restored it.
  useEffect(() => {
    if (state.yaml === '' && source.kind === 'preset') void seed(source.name, ++opSeq.current);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // A pristine preset is valid with no round trip — it is server-supplied and
  // already parses. Anything the reader has a hand in is validated below.
  useEffect(() => {
    if (!needsValidation(edited, source)) onValidityChange(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [edited, source.kind, source.kind === 'preset' ? source.name : '']);

  // Validity is reported ONLY when an answer arrives. Setting it false on every
  // keystroke and true again 400ms later made Next blink the whole time the
  // reader was typing; holding the last answer across the gap keeps it still,
  // and a document that was never valid stays blocked throughout.
  useEffect(() => {
    if (!needsValidation(edited, source)) return;
    if (!doc.trim()) { onValidityChange(false); return; }
    let cancelled = false;
    const yaml = doc;
    const timer = setTimeout(async () => {
      try {
        const result = await api.validateOntology(yaml);
        if (cancelled) return;
        setValidation({ yaml, result });
        onValidityChange(result.ok);
      } catch {
        if (cancelled) return;
        onValidityChange(false);
      }
    }, DEBOUNCE_MS);
    return () => { cancelled = true; clearTimeout(timer); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [doc, edited, source.kind]);

  async function seed(presetName: string, seq: number) {
    const stale = () => !mounted.current || seq !== opSeq.current;
    setSeedBusy(true); setSeedError(''); setFileError('');
    try {
      const yaml = await api.ontologyPresetYAML(presetName);
      if (stale()) return;
      setDoc(yaml);
    } catch (e) {
      if (stale()) return;
      setSeedError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!stale()) setSeedBusy(false);
    }
  }

  // choose applies a source change. Only ever reached once the reader has
  // agreed to lose whatever the editor is holding.
  function choose(value: string) {
    const seq = ++opSeq.current;
    setFileError(''); setSeedError(''); setValidation(null);
    if (value === UPLOAD) { fileInput.current?.click(); return; }
    if (value === BLANK) {
      setSource({ kind: 'blank' });
      setEdited(true);
      setDoc('');
      onDispatch({ type: 'SET_YAML', yaml: '' });
      return;
    }
    setSource({ kind: 'preset', name: value });
    setEdited(false);
    // SET_PRESET clears state.yaml — which is what puts the wire body back on
    // mode 'preset' — and records the choice in seedPreset, where it survives
    // a later SET_YAML.
    onDispatch({ type: 'SET_PRESET', preset: value });
    void seed(value, seq);
  }

  function requestChoose(value: string) {
    if (!value) return;
    if (edited && doc.trim() !== '') { setPending(value); return; }
    choose(value);
  }

  async function handleFile(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    e.target.value = ''; // allow re-selecting the same file after fixing it
    if (!file) return;
    opSeq.current++; // abandons any in-flight seed fetch
    setSeedBusy(false); setSeedError(''); setFileError('');
    if (file.size > MAX_ONTOLOGY_BYTES) {
      setFileError(`That file is ${Math.ceil(file.size / 1024)} KiB — ontologies are capped at 256 KiB.`);
      onValidityChange(false);
      return;
    }
    const text = await file.text();
    if (!mounted.current) return;
    setSource({ kind: 'file', name: file.name });
    setEdited(true);
    setDoc(text);
    setValidation(null);
    onDispatch({ type: 'SET_YAML', yaml: text });
    onValidityChange(false); // present but not yet validated
  }

  // Typing is not a mode change — it is a fact about the document, and the only
  // thing it switches is what gets sent.
  function handleEditorChange(text: string) {
    setDoc(text);
    if (!edited) setEdited(true);
    onDispatch({ type: 'SET_YAML', yaml: text });
  }

  // revert discards the reader's edits, so it is offered rather than performed,
  // and only while there is a preset to revert TO.
  function revert() {
    if (source.kind !== 'preset') return;
    const seq = ++opSeq.current;
    setEdited(false);
    setValidation(null);
    onDispatch({ type: 'SET_PRESET', preset: source.name });
    void seed(source.name, seq);
  }

  const selectValue = source.kind === 'preset' ? source.name : source.kind === 'blank' ? BLANK : UPLOAD;
  // A summary only ever describes the document currently on screen — it used to
  // outlive its subject, reporting a rule count for an ontology whose rules the
  // reader had since deleted.
  const shown = validation && validation.yaml === doc ? validation.result : null;
  const checking = needsValidation(edited, source) && doc.trim() !== '' && !shown && !seedBusy;
  const presetRecord = source.kind === 'preset' ? presets.find(p => p.name === source.name) : undefined;

  return (
    <div data-testid="step-ontology">
      <label style={label}>Ontology</label>
      <p style={hint}>
        The ontology is the topic tree and validation rules new facts are checked
        against.{' '}
        {/* target="_blank": the desktop build is a WKWebView, so an in-frame
            navigation here would strand the reader with no way back. */}
        <a href="https://knomit.io/docs/ontology" target="_blank" rel="noreferrer" style={link}>
          What is an ontology?
        </a>
      </p>

      <div style={sourceRow}>
        <select data-testid="ontology-source-select" style={{ ...input, width: 'auto', minWidth: 200 }}
          value={selectValue} disabled={seedBusy}
          onChange={e => requestChoose(e.target.value)}>
          {presets.map(p => <option key={p.name} value={p.name}>{p.title}</option>)}
          {/* An uploaded file is a source in its own right, and the select has
              to be able to SHOW it — otherwise it snaps back to naming a preset
              the editor is not holding, which is the lie the old header told. */}
          {source.kind === 'file'
            ? <option value={UPLOAD}>{source.name}</option>
            : <option value={UPLOAD}>Upload a file…</option>}
          <option value={BLANK}>Start from an empty document</option>
        </select>

        {pending ? (
          <span data-testid="ontology-replace-confirm" style={{ ...prov, color: '#d2a24c' }}>
            Replace with {sourceLabel(pending, presets)}? Your edits are not kept.{' '}
            <button type="button" data-testid="ontology-replace-yes" style={linkBtn}
              onClick={() => { const v = pending; setPending(''); choose(v); }}>Replace</button>
            <button type="button" data-testid="ontology-replace-no" style={{ ...linkBtn, marginLeft: 10 }}
              onClick={() => setPending('')}>Keep editing</button>
          </span>
        ) : (
          <span data-testid="ontology-source" style={prov}>
            {source.kind === 'file' ? (
              <>Editing <b style={{ color: '#ddd' }}>{source.name}</b></>
            ) : source.kind === 'blank' ? (
              'Writing your own, from an empty document'
            ) : edited ? (
              <>
                <b style={{ color: '#ddd' }}>{sourceLabel(source.name, presets)}</b>
                <span data-testid="ontology-edited" style={{ color: '#7c9' }}> · edited</span>{' '}
                <button type="button" data-testid="ontology-revert" style={linkBtn} onClick={revert}>revert</button>
              </>
            ) : (
              presetRecord?.description ?? ''
            )}
          </span>
        )}
      </div>

      {/* One file input for the step, opened by the select's Upload option — a
          bare <input type=file> cannot be styled to match the rest. */}
      <input ref={fileInput} data-testid="ontology-file" type="file" hidden
        accept=".yaml,.yml,text/yaml" onChange={handleFile} />

      <div style={panel}>
        {seedBusy && <div style={hint}>Loading…</div>}
        {seedError && <div style={warnText}>{seedError}</div>}
        {fileError && <div data-testid="ontology-file-error" style={warnText}>{fileError}</div>}
        {!seedBusy && !seedError && (
          <>
            <OntologyEditor value={doc} onChange={handleEditorChange} />
            {shown
              ? <ValidationSummary result={shown} />
              : checking
                ? <div data-testid="ontology-checking" style={summaryDetail}>Checking…</div>
                : !edited && presetRecord
                  ? (
                    <div data-testid="ontology-preset-summary" style={summary}>
                      <div>{presetRecord.id} — {presetRecord.title}</div>
                      <div style={summaryDetail}>{presetRecord.topics.length} topics</div>
                    </div>
                  )
                  : null}
          </>
        )}
      </div>
    </div>
  );
}

type Source =
  | { kind: 'preset'; name: string }
  | { kind: 'file'; name: string }
  | { kind: 'blank' };

// A pristine preset is the one document the server already vouches for.
// Everything else — edited, uploaded, blank — is the reader's, and is checked.
function needsValidation(edited: boolean, source: Source): boolean {
  return edited || source.kind !== 'preset';
}

function sourceLabel(value: string, presets: OntologyPreset[]): string {
  if (value === BLANK) return 'an empty document';
  if (value === UPLOAD) return 'an uploaded file';
  return presets.find(p => p.name === value)?.title ?? value;
}

// ValidationSummary renders an OntologyValidation result: the parsed
// id/name/topic-count/rule-count on success, or each diagnostic as
// "Line N — message" on failure. Amber is reserved for genuine failure —
// diagnostics qualify; the success summary stays plain text.
function ValidationSummary({ result }: { result: OntologyValidation }) {
  if (result.ok) {
    return (
      <div data-testid="ontology-valid" style={summary}>
        <div>{result.id} — {result.name}</div>
        <div style={summaryDetail}>{result.topics.length} topics</div>
        <div style={summaryDetail}>{result.rule_count} validation rules</div>
      </div>
    );
  }
  return (
    <div data-testid="ontology-diagnostics" style={summary}>
      {result.diagnostics.map((d: OntologyDiagnostic, i: number) => (
        // Line 0 is the server saying it had no position for this problem.
        // Printing "Line 0" would be worse than printing nothing.
        <div key={i} style={warnText}>{d.line > 0 ? `Line ${d.line} — ` : ''}{d.message}</div>
      ))}
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5 };
const link: React.CSSProperties = { color: '#6ea8fe' };
const panel: React.CSSProperties = { marginTop: 12, padding: '10px 12px', background: '#111', border: '1px solid #2a2a2a', borderRadius: 6 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 4 };
const summary: React.CSSProperties = { marginTop: 8, fontSize: 12, color: '#ccc', lineHeight: 1.6 };
const summaryDetail: React.CSSProperties = { color: '#999', fontSize: 12, marginTop: 8 };
const sourceRow: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginTop: 10 };
const prov: React.CSSProperties = { fontSize: 11.5, color: '#999', lineHeight: 1.5 };
const linkBtn: React.CSSProperties = { background: 'none', border: 'none', color: '#6ea8fe', cursor: 'pointer', fontSize: 11.5, padding: 0, textDecoration: 'underline' };
