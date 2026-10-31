import { readChoice, saveChoice, watchChoice } from './ky-ui/theme';
import { busnesPalettes } from './ky-ui/palettes';
import { SYSTEM_THEME, THEME_CATALOG, THEME_PAIRS, THEME_PALETTES, THEME_VARIABLES, findPair, findTheme } from './ky-ui/themes';
export const THEME_STORAGE_KEY = "kypost-theme";

// Saved choices are "system", a pair ID (that pair following the OS) or a half's
// catalog name ("Ocean Dark", a fixed mode). Earlier names ("Ocean", "Current",
// "Cliffs"…) resolve through findTheme; removed ones fall back to system.
export const THEME_OPTIONS = THEME_CATALOG.map((theme) => theme.name);
export type ThemeName = string;

/** The Appearance page and sign-in switcher: per pair, Match system / Light / Dark. */
export const THEME_CHOICES = THEME_PAIRS.map((pair) => ({
  pair,
  choices: [
    { mode: "system", label: "Match system", value: pair.id === "busnes-pair" ? SYSTEM_THEME.id : pair.id },
    { mode: "light", label: "Light", value: findTheme(pair.light)!.name },
    { mode: "dark", label: "Dark", value: findTheme(pair.dark)!.name },
  ],
}));

// Mail-only roles (New Email button and link underline) take the theme's accent,
// button text and line.
const MAIL_KEYS = ["newEmailBorder", "newEmailStart", "newEmailEnd", "newEmailText", "linkBorder"] as const;

export const themes: Record<ThemeName, Record<string, string>> = Object.fromEntries(THEME_CATALOG.map((theme) => {
  const palette: Record<string, string> = { ...(THEME_PALETTES[theme.id as keyof typeof THEME_PALETTES] ?? busnesPalettes[theme.name as keyof typeof busnesPalettes]) };
  const mail = [palette.accent, palette.accent, palette.accent, palette.buttonText, palette.line];
  MAIL_KEYS.forEach((key, i) => { palette[key] = mail[i]; });
  return [theme.name, palette];
}));

const cssName = (key: string) => "--" + key.replace(/[A-Z]/g, (m) => "-" + m.toLowerCase());
const ALL_KEYS = [...Object.keys(THEME_VARIABLES), ...MAIL_KEYS];

function systemTheme(): ThemeName { return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "Busnes Dark" : "Busnes Light"; }

function applyThemeVars(themeName: ThemeName) {
  const root = document.documentElement;
  const theme = findTheme(themeName) ?? findTheme(systemTheme())!;
  for (const key of ALL_KEYS) root.style.removeProperty(cssName(key));
  for (const [key, value] of Object.entries(themes[theme.name])) root.style.setProperty(cssName(key), value);
  root.style.colorScheme = theme.scheme === "dark" ? "dark" : "light";
  if (theme.id === "busnes-light" || theme.id === "busnes-dark") root.dataset.theme = theme.id;
  else delete root.dataset.theme;
}

/** The saved choice: "system", a pair ID or a half's catalog name; nothing saved follows the OS. */
export function getStoredChoice(): ThemeName {
  const saved = readChoice(THEME_STORAGE_KEY);
  const pair = findPair(saved);
  if (pair) return pair.id === "busnes-pair" ? SYSTEM_THEME.id : pair.id;
  const theme = findTheme(saved);
  return theme && theme !== SYSTEM_THEME ? theme.name : SYSTEM_THEME.id;
}

/** The theme on screen as a catalog name: the saved half, or the OS's half of a pair or of Busnes. */
export function getStoredTheme(): ThemeName {
  const choice = getStoredChoice();
  if (choice === SYSTEM_THEME.id) return systemTheme();
  return findTheme(choice)!.name;
}

/** Applies and saves "system", a pair ID or a catalog name. */
export function applyTheme(choice: ThemeName) {
  applyThemeVars(choice === SYSTEM_THEME.id ? systemTheme() : choice);
  saveChoice(THEME_STORAGE_KEY, choice);
}

export function applyStoredTheme() {
  applyThemeVars(getStoredTheme());
}

if (typeof window !== 'undefined') watchChoice(THEME_STORAGE_KEY, applyStoredTheme);
