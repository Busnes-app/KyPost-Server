// Generated from ky-ui themes.js; refresh with sync-consumers.mjs.
window.kyThemes = (() => {
'use strict';
// Suite theme catalog: twelve halves in six light/dark pairs. IDs and names are saved by
// products; never rename one. A renamed half keeps its old name as an alias.
// Busnes values live only in tokens.css. Nord keeps its base CSS colours: sidebar = panel,
// glow = transparent, the base button text fallback and its own status colours. Forest's
// success is teal, because the shared green sits too close to its green accent.
const SYSTEM_THEME = Object.freeze({ id: 'system', name: 'Match system', scheme: 'system', aliases: Object.freeze([]) });

const entry = (id, name, scheme, pair, aliases = []) => Object.freeze({ id, name, scheme, pair, aliases: Object.freeze(aliases) });

// Removed IDs (tropics, tropic-night, coffee, cyber-punk, neon-purple, space, sky, sun, cyber,
// oled) resolve to nothing, so products fall back to Busnes following the OS.
const THEME_CATALOG = Object.freeze([
  entry('busnes-light', 'Busnes Light', 'light', 'busnes-pair'),
  entry('busnes-dark', 'Busnes Dark', 'dark', 'busnes-pair'),
  entry('light-matter', 'Matter Light', 'light', 'matter-pair', ['Light Matter']),
  entry('dark-matter', 'Matter Dark', 'dark', 'matter-pair', ['Dark Matter', 'Current']),
  entry('white-cliffs', 'Ocean Light', 'light', 'ocean-pair', ['White Cliffs', 'Cliffs']),
  entry('ocean', 'Ocean Dark', 'dark', 'ocean-pair', ['Ocean']),
  entry('forest-light', 'Forest Light', 'light', 'forest-pair'),
  entry('forest', 'Forest Dark', 'dark', 'forest-pair', ['Forest']),
  entry('polished-ky', 'Ky Light', 'light', 'ky-pair', ['Polished Ky']),
  entry('patina-ky', 'Ky Dark', 'dark', 'ky-pair', ['Patina Ky', 'patina', 'Patina Ky (base)']),
  entry('paper', 'Nord Light', 'light', 'nord-pair', ['Paper Clean']),
  entry('nord', 'Nord Dark', 'dark', 'nord-pair', ['Nord', 'Nord Slate']),
]);

// A saved pair ID follows the OS scheme. Pair IDs end in `-pair`, which no half ID does.
const pair = (id, name, light, dark) => Object.freeze({ id, name, light, dark });
const THEME_PAIRS = Object.freeze([
  pair('busnes-pair', 'Busnes', 'busnes-light', 'busnes-dark'),
  pair('matter-pair', 'Matter', 'light-matter', 'dark-matter'),
  pair('ocean-pair', 'Ocean', 'white-cliffs', 'ocean'),
  pair('forest-pair', 'Forest', 'forest-light', 'forest'),
  pair('ky-pair', 'Ky', 'polished-ky', 'patina-ky'),
  pair('nord-pair', 'Nord', 'paper', 'nord'),
]);

// Suite palettes never defined hover or strong lines; derive them as Busnes does
// (panel a step toward ink, line strengthened). Base themes pass their actual values,
// plus status colours. Palettes without a status colour use the shared semantic
// default for their own scheme, including when it differs from the OS scheme.
const p = (bg, panel, ink, inkStrong, accent, accentSoft, line, glow, sidebarStart, sidebarEnd, buttonText,
  panelHover = `color-mix(in srgb, ${panel} 93%, ${inkStrong})`, lineStrong = `color-mix(in srgb, ${line} 60%, ${inkStrong})`, danger, success) =>
  Object.freeze({ bg, panel, ink, inkStrong, accent, accentSoft, line, glow, sidebarStart, sidebarEnd, buttonText, panelHover, lineStrong, ...(danger && { danger }), ...(success && { success }) });

// Every named theme except Busnes Light/Dark.
const THEME_PALETTES = Object.freeze({
  'light-matter': p('#f5efe5', '#fff8ee', '#4c3d32', '#2d1f15', '#805a39', '#e2d0b8', '#d4c7b5', 'transparent', '#ede2d2', '#ede2d2', '#ffffff'),
  'dark-matter': p('#1d1b19', '#292725', '#cfc6bb', '#f0e9df', '#c29a72', '#49392c', '#45403a', 'transparent', '#25221f', '#25221f', '#24170f'),
  'white-cliffs': p('#f7f9fb', '#ffffff', '#2e4c63', '#163246', '#1d5fa6', '#dde9f7', '#c7d6df', 'transparent', '#eaf1f8', '#eaf1f8', '#ffffff'),
  ocean: p('#0f1b24', '#152a36', '#b8d8e8', '#e0f2fb', '#6aa8e8', '#1f3a55', '#2f5567', 'rgba(58, 130, 155, 0.24)', '#112430', '#173342', '#0a1b22'),
  'forest-light': p('#f3f6ef', '#fcfdf9', '#435342', '#203322', '#4b6b37', '#e2ebda', '#c5d0bc', 'transparent', '#e8eee1', '#e8eee1', '#ffffff', undefined, undefined, undefined, '#00737a'),
  forest: p('#142018', '#1f2f24', '#c7dbc7', '#e3f0df', '#8faa74', '#3a5837', '#4f694f', 'rgba(118, 148, 95, 0.24)', '#18261c', '#223629', '#12200f', undefined, undefined, undefined, '#6ee0d0'),
  'polished-ky': p('#eef2f6', '#ffffff', '#475569', '#0f172a', '#08758b', '#daf2f4', '#c2cdd8', 'transparent', '#e7edf3', '#e7edf3', '#ffffff'),
  'patina-ky': p('#10161e', '#1b2530', '#afbdca', '#e2e8f0', '#6fc9cc', '#203d41', '#34414d', 'transparent', '#161e27', '#161e27', '#10161e'),
  paper: p('#eceff4', '#ffffff', '#4c566a', '#2e3440', '#4a6a94', '#dbe4ef', '#c5cdd9', 'transparent', '#e5e9f0', '#e5e9f0', '#ffffff'),
  nord: p('#2e3440', '#3b4252', '#d8dee9', '#eceff4', '#88c0d0', 'rgba(136, 192, 208, 0.2)', '#4c566a', 'transparent', '#3b4252', '#3b4252', '#0d0f14', '#434c5e', '#5e81ac', '#ecc4c8', '#bcd4aa'),
});

// Product properties managed by palette application, including scheme-bound warning.
const THEME_VARIABLES = Object.freeze(Object.fromEntries(
  [...Object.keys(THEME_PALETTES.nord), 'warning'].map(key => [key, '--' + key.replace(/[A-Z]/g, c => '-' + c.toLowerCase())]),
));

const prefersDark = () => globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false;

/** Returns the pair for a saved pair ID, or undefined. */
function findPair(value) {
  return THEME_PAIRS.find(pair => pair.id === value);
}

/**
 * Returns the catalog entry for a saved ID, name or alias, or undefined. A pair ID
 * resolves to its half for the given scheme, by default the OS's.
 */
function findTheme(value, dark = prefersDark()) {
  if (value === SYSTEM_THEME.id) return SYSTEM_THEME;
  const pair = findPair(value);
  if (pair) value = dark ? pair.dark : pair.light;
  return THEME_CATALOG.find(theme => theme.id === value || theme.name === value || theme.aliases.includes(value));
}

/**
 * Sets a named palette's custom properties and color-scheme on root, or clears both
 * for system and Busnes so tokens.css applies. A pair ID applies its half for the OS
 * scheme. Returns the scheme, or null when cleared. Uses the CSSOM, which `style-src 'self'` permits.
 */
function applyThemeVariables(value, root = globalThis.document?.documentElement) {
  const theme = findTheme(value);
  const palette = THEME_PALETTES[theme?.id];
  for (const name of Object.values(THEME_VARIABLES)) root.style.removeProperty(name);
  root.style.colorScheme = '';
  if (!palette) return null;
  for (const [key, name] of Object.entries(THEME_VARIABLES)) if (palette[key]) root.style.setProperty(name, palette[key]);
  root.style.setProperty('--danger', palette.danger ?? `var(--ky-danger-${theme.scheme})`);
  root.style.setProperty('--success', palette.success ?? `var(--ky-success-${theme.scheme})`);
  root.style.setProperty('--warning', `var(--ky-warning-${theme.scheme})`);
  root.style.colorScheme = theme.scheme;
  return theme.scheme;
}

return Object.freeze({ SYSTEM_THEME, THEME_CATALOG, THEME_PAIRS, THEME_PALETTES, THEME_VARIABLES, findPair, findTheme, applyThemeVariables });
})();
