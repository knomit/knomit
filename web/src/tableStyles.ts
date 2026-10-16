// The facet tables' shared styles: the sessions table (SessionsTable) and the
// fleet's Members and Activity, one look for a table on a facet.

// The only thing that gives when the body is short: it takes what is left and
// scrolls both axes (the table is wider than the body on a narrow window).
export const tableWrap: React.CSSProperties = {
  flex: 1, minHeight: 0, overflow: 'auto', padding: '0 12px',
};
// A header cell, sticky on the CELL (the broadly supported form, WKWebView
// included), its rule an inset shadow that travels with it.
export const th: React.CSSProperties = {
  position: 'sticky', top: 0, background: '#111',
  boxShadow: 'inset 0 -1px 0 #222', padding: '2px 0',
};
// The table's footer. Pinned under the scroller (flexShrink 0), never inside it.
export const policyFooter: React.CSSProperties = {
  color: '#666', fontSize: 11, flexShrink: 0,
  margin: 0, padding: '8px 12px', borderTop: '1px solid #1c1c1c',
};
