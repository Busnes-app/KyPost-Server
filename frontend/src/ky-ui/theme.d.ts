export function readChoice(key: string): string | null;
export function saveChoice(key: string, value: string): void;
export function watchChoice(key: string, apply: () => void, hasChoice: () => boolean): () => void;
export type ThemeChoice = 'system' | 'busnes-light' | 'busnes-dark';
export const THEMES: readonly ThemeChoice[];
export const THEME_STORAGE_KEY: 'ky-theme';
export function normalizeTheme(value: unknown): ThemeChoice;
export function resolveTheme(value: unknown, prefersDark?: boolean): Exclude<ThemeChoice, 'system'>;
export function readStoredTheme(storage?: Partial<Storage> | null): ThemeChoice;
export function applyTheme(choice?: unknown, options?: {
  document?: Document;
  storage?: Partial<Storage> | null;
  prefersDark?: boolean;
  persist?: boolean;
}): ThemeChoice;
