export type ThemeId =
  | 'busnes-light' | 'busnes-dark' | 'dark-matter' | 'light-matter' | 'tropics' | 'tropic-night'
  | 'ocean' | 'coffee' | 'white-cliffs' | 'cyber-punk' | 'neon-purple' | 'space' | 'sky' | 'forest'
  | 'sun' | 'patina-ky' | 'polished-ky' | 'patina' | 'cyber' | 'nord' | 'paper' | 'oled';
export type NamedThemeId = Exclude<ThemeId, 'busnes-light' | 'busnes-dark'>;
export interface CatalogTheme {
  readonly id: ThemeId;
  readonly name: string;
  readonly scheme: 'light' | 'dark';
  readonly aliases: readonly string[];
}
export interface SystemTheme {
  readonly id: 'system';
  readonly name: 'Match system';
  readonly scheme: 'system';
  readonly aliases: readonly string[];
}
export interface ThemePalette {
  readonly bg: string;
  readonly panel: string;
  readonly ink: string;
  readonly inkStrong: string;
  readonly accent: string;
  readonly accentSoft: string;
  readonly line: string;
  readonly glow: string;
  readonly sidebarStart: string;
  readonly sidebarEnd: string;
  readonly buttonText: string;
  readonly panelHover: string;
  readonly lineStrong: string;
  /** Base-family themes only. */
  readonly danger?: string;
  readonly success?: string;
}
export const SYSTEM_THEME: SystemTheme;
export const THEME_CATALOG: readonly CatalogTheme[];
export const THEME_PALETTES: Readonly<Record<NamedThemeId, ThemePalette>>;
export const THEME_VARIABLES: Readonly<Record<keyof ThemePalette | 'warning', string>>;
export function findTheme(value: unknown): CatalogTheme | SystemTheme | undefined;
export function applyThemeVariables(value: unknown, root?: HTMLElement): 'light' | 'dark' | null;
