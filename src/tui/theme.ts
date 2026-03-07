export interface Theme {
  primary: string;
  secondary: string;
  highlight: string;
  dimmed: string;
  error: string;
  success: string;
}

export const defaultTheme: Theme = {
  primary: "magenta",
  secondary: "cyan",
  highlight: "magentaBright",
  dimmed: "gray",
  error: "red",
  success: "green",
};
