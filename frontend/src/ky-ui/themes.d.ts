export type ThemeId =
  | 'busnes-light' | 'busnes-dark' | 'light-matter' | 'dark-matter' | 'white-cliffs' | 'ocean'
  | 'forest-light' | 'forest' | 'polished-ky' | 'patina-ky' | 'paper' | 'nord';
export type PairId = 'busnes-pair' | 'matter-pair' | 'ocean-pair' | 'forest-pair' | 'ky-pair' | 'nord-pair';
export type NamedThemeId = Exclude<ThemeId, 'busnes-light' | 'busnes-dark'>;
export interface CatalogTheme {
  readonly id: ThemeId;
  readonly name: string;
  readonly scheme: 'light' | 'dark';
  readonly pair: PairId;
  readonly aliases: readonly string[];
}
export interface ThemePair {
  readonly id: PairId;
  readonly name: string;
  readonly light: ThemeId;
  readonly dark: ThemeId;
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
export const THEME_PAIRS: readonly ThemePair[];
export const THEME_PALETTES: Readonly<Record<NamedThemeId, ThemePalette>>;
export const THEME_VARIABLES: Readonly<Record<keyof ThemePalette | 'warning', string>>;
export function findPair(value: unknown): ThemePair | undefined;
/** A pair ID resolves to its half for `dark`, by default the OS scheme. */
export function findTheme(value: unknown, dark?: boolean): CatalogTheme | SystemTheme | undefined;
export function applyThemeVariables(value: unknown, root?: HTMLElement): 'light' | 'dark' | null;
