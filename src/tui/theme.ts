export interface Theme {
  primary: string;
  secondary: string;
  highlight: string;
  dimmed: string;
  error: string;
  success: string;
  accent: string;
  muted: string;
}

export const defaultTheme: Theme = {
  primary: "#c678dd",      // soft purple
  secondary: "#61afef",    // soft blue
  highlight: "#e5c07b",    // warm gold
  dimmed: "gray",
  error: "#e06c75",        // soft red
  success: "#98c379",      // soft green
  accent: "#56b6c2",       // teal
  muted: "#5c6370",        // dark gray
};

// Box-drawing characters for custom borders
export const box = {
  topLeft: "╭",
  topRight: "╮",
  bottomLeft: "╰",
  bottomRight: "╯",
  horizontal: "─",
  vertical: "│",
  teeRight: "├",
  teeLeft: "┤",
  teeDown: "┬",
  teeUp: "┴",
  cross: "┼",
} as const;

// UI glyphs
export const glyph = {
  cursor: "▸",
  world: "◆",
  fact: "◇",
  breadcrumbSep: "›",
  bullet: "•",
  timelineDot: "●",
  timelineLine: "│",
  tag: "⌗",
  confidence: "◈",
  branch: "⎇",
  search: "⌕",
  empty: "∅",
  arrow: "→",
} as const;
