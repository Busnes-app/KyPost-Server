import { readChoice, saveChoice, watchChoice } from './ky-ui/theme';
import { busnesPalettes } from './ky-ui/palettes';
import { SYSTEM_THEME, THEME_CATALOG, THEME_PALETTES, THEME_VARIABLES, findTheme } from './ky-ui/themes';
export const THEME_STORAGE_KEY = "kypost-theme";

// Saved choices are catalog names ("Ocean"), the earlier aliases findTheme maps
// ("Current", "Cliffs"…) or "system". ky-ui themes.js owns every palette.
export const THEME_OPTIONS = THEME_CATALOG.map((theme) => theme.name);
export type ThemeName = string;

// Mail-only roles: New Email button and link underline. Themes not listed take
// them from their accent, button text and line, as Busnes does.
const MAIL_KEYS = ["newEmailBorder", "newEmailStart", "newEmailEnd", "newEmailText", "linkBorder"] as const;
const MAIL: Record<string, readonly string[]> = {
  "Dark Matter": ["#8f6b4a", "#c29a72", "#9a7450", "#24170f", "#d8dde6"],
  "Light Matter": ["#8f6b4a", "#c29a72", "#9a7450", "#24170f", "#b7a38c"],
  "Tropics": ["#78a100", "#9bc400", "#7ea100", "#243100", "#bcb0a0"],
  "Tropic Night": ["#78a100", "#9bc400", "#7ea100", "#1a2400", "#7f7599"],
  "Ocean": ["#4f91a6", "#74bacd", "#4f91a6", "#0a1b22", "#7ba7b8"],
  "Coffee": ["#8f5f42", "#b47f5c", "#8f5f42", "#220f08", "#8f7a6d"],
  "White Cliffs": ["#2f7fb0", "#4f9bc8", "#58b65a", "#103246", "#58b65a"],
  "Cyber Punk": ["#00c9ad", "#00f5d4", "#00c9ad", "#051d1a", "#c38fdd"],
  "Neon Purple": ["#9d45d3", "#c86cff", "#9d45d3", "#210a35", "#b78ed9"],
  "Space": ["#6788dd", "#86a8ff", "#6788dd", "#101930", "#8ca0c8"],
  "Sky": ["#4f93b8", "#6db3d6", "#4f93b8", "#0f2e3f", "#89afc2"],
  "Forest": ["#6f8d5a", "#8faa74", "#6f8d5a", "#12200f", "#90a98d"],
  "Sun": ["#bb8631", "#e0ab4f", "#bb8631", "#2a1808", "#caa670"],
  "Patina Ky": ["#0e9668", "#4deeea", "#10b981", "#04120d", "#94a3b8"],
  "Polished Ky": ["#059669", "#0891b2", "#10b981", "#042f2e", "#64748b"],
};

export const themes: Record<ThemeName, Record<string, string>> = Object.fromEntries(THEME_CATALOG.map((theme) => {
  const palette: Record<string, string> = { ...(THEME_PALETTES[theme.id as keyof typeof THEME_PALETTES] ?? busnesPalettes[theme.name as keyof typeof busnesPalettes]) };
  const mail = MAIL[theme.name] ?? [palette.accent, palette.accent, palette.accent, palette.buttonText, palette.line];
  MAIL_KEYS.forEach((key, i) => { palette[key] = mail[i]; });
  return [theme.name, palette];
}));

const cssName = (key: string) => "--" + key.replace(/[A-Z]/g, (m) => "-" + m.toLowerCase());
const ALL_KEYS = [...Object.keys(THEME_VARIABLES), ...MAIL_KEYS];

function systemTheme(): ThemeName { return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "Busnes Dark" : "Busnes Light"; }

function applyThemeVars(themeName: ThemeName) {
  const root = document.documentElement;
  const theme = findTheme(themeName);
  for (const key of ALL_KEYS) root.style.removeProperty(cssName(key));
  for (const [key, value] of Object.entries(themes[themeName])) root.style.setProperty(cssName(key), value);
  root.style.colorScheme = theme?.scheme === "dark" ? "dark" : "light";
  if (theme?.id === "busnes-light" || theme?.id === "busnes-dark") root.dataset.theme = theme.id;
  else delete root.dataset.theme;
}

/** The saved choice as a catalog name or "system"; nothing saved follows the OS. */
export function getStoredChoice(): ThemeName {
  const theme = findTheme(readChoice(THEME_STORAGE_KEY));
  return theme ? theme.name === SYSTEM_THEME.name ? SYSTEM_THEME.id : theme.name : SYSTEM_THEME.id;
}

/** The theme on screen: the saved one, or Busnes Light/Dark from the OS. */
export function getStoredTheme(): ThemeName {
  const choice = getStoredChoice();
  return choice === SYSTEM_THEME.id ? systemTheme() : choice;
}

/** Applies and saves a catalog name or "system". */
export function applyTheme(choice: ThemeName) {
  applyThemeVars(choice === SYSTEM_THEME.id ? systemTheme() : choice);
  saveChoice(THEME_STORAGE_KEY, choice);
}

export function applyStoredTheme() {
  applyThemeVars(getStoredTheme());
}

if (typeof window !== 'undefined') watchChoice(THEME_STORAGE_KEY, applyStoredTheme, () => getStoredChoice() !== SYSTEM_THEME.id);
