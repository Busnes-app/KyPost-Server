// Suite theme catalog. IDs and names are saved by products; never rename one.
// Busnes values live only in tokens.css. Base-family themes keep their saved IDs (patina,
// cyber, nord, paper, oled) and their base CSS colours: sidebar = panel, glow = transparent
// and the base button text fallback. Base `patina` is not suite Patina Ky; both stay.
export const SYSTEM_THEME = Object.freeze({ id: 'system', name: 'Match system', scheme: 'system', aliases: Object.freeze([]) });

const entry = (id, name, scheme, aliases = []) => Object.freeze({ id, name, scheme, aliases: Object.freeze(aliases) });

export const THEME_CATALOG = Object.freeze([
  entry('busnes-light', 'Busnes Light', 'light'),
  entry('busnes-dark', 'Busnes Dark', 'dark'),
  entry('dark-matter', 'Dark Matter', 'dark', ['Current']),
  entry('light-matter', 'Light Matter', 'light'),
  entry('tropics', 'Tropics', 'light', ['Old Light']),
  entry('tropic-night', 'Tropic Night', 'dark', ['Old Dark']),
  entry('ocean', 'Ocean', 'dark'),
  entry('coffee', 'Coffee', 'dark'),
  entry('white-cliffs', 'White Cliffs', 'light', ['Cliffs']),
  entry('cyber-punk', 'Cyber Punk', 'dark'),
  entry('neon-purple', 'Neon Purple', 'dark'),
  entry('space', 'Space', 'dark'),
  entry('sky', 'Sky', 'light'),
  entry('forest', 'Forest', 'dark'),
  entry('sun', 'Sun', 'light'),
  entry('patina-ky', 'Patina Ky', 'dark'),
  entry('polished-ky', 'Polished Ky', 'light'),
  entry('patina', 'Patina Ky (base)', 'dark'),
  entry('cyber', 'Cyber Dark', 'dark'),
  entry('nord', 'Nord Slate', 'dark'),
  entry('paper', 'Paper Clean', 'light'),
  entry('oled', 'OLED Black', 'dark'),
]);

// Suite palettes never defined hover or strong lines; derive them as Busnes does
// (panel a step toward ink, line strengthened). Base themes pass their actual values,
// plus status colours. Palettes without status colours use the shared semantic
// defaults for their own scheme, including when it differs from the OS scheme.
const p = (bg, panel, ink, inkStrong, accent, accentSoft, line, glow, sidebarStart, sidebarEnd, buttonText,
  panelHover = `color-mix(in srgb, ${panel} 93%, ${inkStrong})`, lineStrong = `color-mix(in srgb, ${line} 60%, ${inkStrong})`, danger, success) =>
  Object.freeze({ bg, panel, ink, inkStrong, accent, accentSoft, line, glow, sidebarStart, sidebarEnd, buttonText, panelHover, lineStrong, ...(danger && { danger, success }) });

// Every named theme except Busnes Light/Dark.
export const THEME_PALETTES = Object.freeze({
  'dark-matter': p('#1a1a1e', '#252530', '#d4c5e2', '#e8ddf5', '#c29a72', '#5a3f31', '#404050', 'rgba(107, 74, 66, 0.25)', '#1f1f24', '#2a2530', '#24170f'),
  'light-matter': p('#f5efe5', '#fff8ee', '#4c3d32', '#2d1f15', '#c29a72', '#e6d2be', '#c5b29d', 'rgba(175, 126, 92, 0.2)', '#ede2d2', '#e4d6c3', '#24170f'),
  tropics: p('#f4f1eb', '#fffaf0', '#43362d', '#241a14', '#9bc400', '#d4e3a0', '#c4b7a3', 'rgba(123, 165, 31, 0.2)', '#ece5d8', '#e3dacb', '#243100'),
  'tropic-night': p('#15131a', '#221f2b', '#cdbde0', '#e8ddf5', '#9bc400', '#6b4a42', '#3c3650', 'rgba(107, 74, 66, 0.28)', '#1d1a24', '#292233', '#1a2400'),
  ocean: p('#0f1b24', '#152a36', '#b8d8e8', '#e0f2fb', '#5ea9be', '#214657', '#2f5567', 'rgba(58, 130, 155, 0.24)', '#112430', '#173342', '#0a1b22'),
  coffee: p('#1d1714', '#2a211d', '#d6c0b3', '#f0ded2', '#b47f5c', '#5f3f2f', '#4a3830', 'rgba(132, 86, 61, 0.24)', '#231a16', '#32251f', '#220f08'),
  'white-cliffs': p('#f7f9fb', '#ffffff', '#2e4c63', '#163246', '#5ea8d8', '#dff1fb', '#8fc3df', 'rgba(94, 168, 216, 0.2)', '#f1f8fd', '#e7f3fb', '#103246'),
  'cyber-punk': p('#120918', '#1e1028', '#f5d0ff', '#ffe9ff', '#00f5d4', '#3b1760', '#5c2d84', 'rgba(255, 0, 153, 0.2)', '#1b0d24', '#281236', '#051d1a'),
  'neon-purple': p('#130b1d', '#231233', '#e4ccff', '#f2e6ff', '#c86cff', '#47206c', '#63358a', 'rgba(200, 108, 255, 0.2)', '#1b1029', '#2a1740', '#210a35'),
  space: p('#0b0f1a', '#151c2d', '#c8d5f0', '#e7efff', '#86a8ff', '#263e74', '#34496f', 'rgba(92, 126, 220, 0.18)', '#0f1625', '#18233a', '#101930'),
  sky: p('#dff1ff', '#f4fbff', '#2f4f64', '#183142', '#6db3d6', '#b6dced', '#93bdd2', 'rgba(109, 179, 214, 0.28)', '#d3ecfa', '#c2e2f4', '#0f2e3f'),
  forest: p('#142018', '#1f2f24', '#c7dbc7', '#e3f0df', '#8faa74', '#3a5837', '#4f694f', 'rgba(118, 148, 95, 0.24)', '#18261c', '#223629', '#12200f'),
  sun: p('#fff3dc', '#fff9ec', '#5a4024', '#392611', '#e0ab4f', '#f1d9a2', '#d4b27a', 'rgba(224, 171, 79, 0.28)', '#f8e7c5', '#f2dab1', '#2a1808'),
  'patina-ky': p('#0d0f14', '#161a22', '#94a3b8', '#e2e8f0', '#4deeea', '#0e4a48', '#1e293b', 'rgba(77, 238, 234, 0.22)', '#0d0f14', '#1b212c', '#04120d'),
  'polished-ky': p('#eef2f6', '#ffffff', '#475569', '#0f172a', '#0891b2', '#cffafe', '#cbd5e1', 'rgba(8, 145, 178, 0.18)', '#f1f5f9', '#e2e8f0', '#042f2e'),
  patina: p('#0d0f14', '#161a22', '#94a3b8', '#f1f5f9', '#4deeea', 'rgba(77, 238, 234, 0.15)', '#1e293b', 'transparent', '#161a22', '#161a22', '#0d0f14', '#1c222c', '#334155', '#f15757', '#10b981'),
  cyber: p('#05070a', '#0d121d', '#aab7c8', '#eef6f5', '#00d9bd', 'rgba(0, 255, 204, 0.18)', '#1e293b', 'transparent', '#0d121d', '#0d121d', '#0d0f14', '#131b2a', '#00ffcc44', '#ff1a66', '#00ff66'),
  nord: p('#2e3440', '#3b4252', '#d8dee9', '#eceff4', '#88c0d0', 'rgba(136, 192, 208, 0.2)', '#4c566a', 'transparent', '#3b4252', '#3b4252', '#0d0f14', '#434c5e', '#5e81ac', '#dfb0b5', '#acc598'),
  paper: p('#f8fafc', '#ffffff', '#64748b', '#0f172a', '#0284c7', 'rgba(2, 132, 199, 0.12)', '#e2e8f0', 'transparent', '#ffffff', '#ffffff', '#0d0f14', '#f1f5f9', '#cbd5e1', '#d12424', '#117a38'),
  oled: p('#000000', '#111111', '#a1a1aa', '#fafafa', '#4deeea', 'rgba(77, 238, 234, 0.15)', '#222222', 'transparent', '#111111', '#111111', '#0d0f14', '#1a1a1a', '#333333', '#ef4444', '#10b981'),
});

// Product properties managed by palette application, including scheme-bound warning.
export const THEME_VARIABLES = Object.freeze(Object.fromEntries(
  [...Object.keys(THEME_PALETTES.patina), 'warning'].map(key => [key, '--' + key.replace(/[A-Z]/g, c => '-' + c.toLowerCase())]),
));

/** Returns the catalog entry for a saved ID, name or legacy alias, or undefined. */
export function findTheme(value) {
  if (value === SYSTEM_THEME.id) return SYSTEM_THEME;
  return THEME_CATALOG.find(theme => theme.id === value || theme.name === value || theme.aliases.includes(value));
}

/**
 * Sets a named palette's custom properties and color-scheme on root, or clears both
 * for system and Busnes so tokens.css applies. Returns the scheme, or null when cleared.
 * Uses the CSSOM, which `style-src 'self'` permits.
 */
export function applyThemeVariables(value, root = globalThis.document?.documentElement) {
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
