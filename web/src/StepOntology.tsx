import { useEffect, useState } from 'react';
import { api, type OntologyPreset } from './api';
import type { WizardAction, WizardState } from './wizardState';

// PLACEHOLDER — Task 9 replaces this wholesale with upload + editor support
// (see task-9-brief.md: StepOntology grows an "Upload a file" and "Write your
// own" path, validation round-trips, and onValidityChange gating). This
// version renders only the preset cards from api.ontologyPresets(), with
// `default` preselected via wizardState's own initial value — no upload, no
// editor, no validation. It exists so Task 8's wizard has an 'ontology' step
// to land on at all, and so Task 9 has one file to expand rather than a hole
// in the step switch.
export function StepOntology({ state, dispatch }: { state: WizardState; dispatch: (a: WizardAction) => void }) {
  const [presets, setPresets] = useState<OntologyPreset[]>([]);

  useEffect(() => {
    let cancelled = false;
    api.ontologyPresets().then(p => { if (!cancelled) setPresets(p); }).catch(() => {});
    return () => { cancelled = true; };
  }, []);

  return (
    <div data-testid="step-ontology">
      <label style={label}>Ontology</label>
      <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', marginTop: 8 }}>
        {presets.map(p => (
          <button key={p.name} type="button" style={presetCard(state.preset === p.name)}
            onClick={() => dispatch({ type: 'SET_PRESET', preset: p.name })}>
            <div style={presetTitle}>{p.title}</div>
            <div style={presetBody}>{p.description}</div>
            <div style={presetTopics}>{p.topics.join(', ')}</div>
          </button>
        ))}
      </div>
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
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
