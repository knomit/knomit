import type React from 'react';
import { repoHueBg, repoHueBorder } from './utils';

// The Connect flow's page grammar (crumb, head, blocks, the progress rail),
// shared by RemoteConnectWizard and the fleet's Point at the new host flow
// (FleetFollowFlow), so the two read as one page. Kept apart from the
// components for react-refresh.

export const crumb: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 7, fontSize: 12, color: '#888', marginBottom: 10,
};
export const crumbLink = (disabled: boolean): React.CSSProperties => ({
  background: 'none', border: 'none', padding: 0, fontSize: 12,
  color: disabled ? '#555' : '#6ea8fe', cursor: disabled ? 'default' : 'pointer',
});
export const crumbSep: React.CSSProperties = { color: '#444' };

export const pageHead: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 };
// Tinted in the repo's own hue rather than a generic accent — this page belongs
// to that repository, and the icon box is where every other detail head says so.
export const iconBox = (repo: string): React.CSSProperties => ({
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: repoHueBg(repo), border: '1px solid ' + repoHueBorder(repo),
  display: 'flex', alignItems: 'center', justifyContent: 'center',
});

// Same two-column split as SettingsPage, to the pixel: the rail must not move
// when you cross from the repo's settings into this page.
export const grid: React.CSSProperties = {
  display: 'grid', gap: 26, alignItems: 'start',
};
// Class names carry the two-column template and its phone-width stack (App.css
// .k-connect-grid): an inline style cannot hold a media query.
export const gridClass = 'k-connect-grid';
export const railClass = 'k-connect-rail';
// A form reads worse the wider it gets, so the column is measured inside its
// grid cell rather than filling it.
export const column: React.CSSProperties = { maxWidth: 470, minWidth: 0 };

export const block: React.CSSProperties = {
  borderTop: '1px solid #202020', paddingTop: 16, marginTop: 16,
  display: 'flex', flexDirection: 'column', gap: 10,
};
export const blockHead: React.CSSProperties = { display: 'flex', alignItems: 'baseline', gap: 10, flexWrap: 'wrap' };
export const blockTitle: React.CSSProperties = { margin: 0, fontSize: 13.5, fontWeight: 650, color: '#e6e6e6' };
export const blockHint: React.CSSProperties = { fontSize: 11.5, color: '#6e6e6e' };

// Actions sit under the content they act on. The old footer was pinned to the
// window's bottom edge, which put "Test connection" 600px below the field it
// tests.
export const actions: React.CSSProperties = { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginTop: 4 };

export const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 10, display: 'block' };
export const input: React.CSSProperties = {
  width: '100%', boxSizing: 'border-box', background: '#0c0c0c', border: '1px solid #333',
  color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13,
};
// ── progress rail ──
// Sticky, same slot and same type sizes as SettingsPage's contents rail. It
// keeps that rail's single green accent rather than importing a second one:
// green already means "here" in this column, and a step tracker that introduced
// blue would be saying the same thing in a new colour.
export const rail: React.CSSProperties = {
  position: 'sticky', top: 0, display: 'flex', flexDirection: 'column', gap: 2, marginTop: 16,
};
export const railLabel: React.CSSProperties = {
  fontSize: 9.5, letterSpacing: '0.13em', textTransform: 'uppercase', color: '#4e4e4e', padding: '0 8px 6px',
};
export type StepState = 'active' | 'done' | 'todo';
export const railItem = (state: StepState): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 8, width: '100%',
  padding: '4px 9px', borderRadius: 4, fontSize: 11.5, textAlign: 'left',
  background: state === 'active' ? '#1a231d' : 'transparent',
  color: state === 'active' ? '#dfe9e2' : state === 'done' ? '#7f9a89' : '#5e5e5e',
  boxShadow: state === 'active' ? 'inset 2px 0 0 -0.5px #7c9' : 'none',
});
// A 15px disc so the numerals sit on the rail's own type scale — the old 22px
// dots were dialog furniture and would tower over an 11.5px label.
export const railMark = (state: StepState): React.CSSProperties => ({
  width: 15, height: 15, borderRadius: '50%', flexShrink: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  fontSize: 9, fontWeight: 700,
  background: state === 'active' ? '#24352b' : state === 'done' ? '#1c2a20' : '#1c1c1c',
  border: '1px solid ' + (state === 'active' ? '#3e5c4a' : state === 'done' ? '#2e4636' : '#2c2c2c'),
  color: state === 'todo' ? '#666' : state === 'done' ? '#7c9' : '#dfe9e2',
});
// The safety line the flow never stated. It is a whole-page fact rather than a
// per-step one, so it lives under the tracker instead of beside a button.
export const railNote: React.CSSProperties = {
  margin: '12px 9px 0', paddingTop: 10, borderTop: '1px solid #242424',
  fontSize: 10.5, color: '#5e5e5e', lineHeight: 1.5,
};
