export interface Theme {
  primary: string;
  secondary: string;
  text: string;
  dim: string;
  surface: string;
  green: string;
  blue: string;
  yellow: string;
  red: string;
  accent: string;
  dark: string;
  crust: string;
  mantle: string;
  base: string;
}

export const defaultTheme: Theme = {
  primary: "#cba6f7",
  secondary: "#89b4fa",
  text: "#cdd6f4",
  dim: "#a6adc8",
  surface: "#45475a",
  green: "#a6e3a1",
  blue: "#89b4fa",
  yellow: "#f9e2af",
  red: "#f38ba8",
  accent: "#94e2d5",
  dark: "#11111b",
  crust: "#181926",
  mantle: "#1e2030",
  base: "#24273a",
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
  activeIndicator: "▌",
  dashDivider: "╌",
} as const;
