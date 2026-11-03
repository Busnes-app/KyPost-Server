/** The script and stylesheet URLs an HTML document names, in order, parsed with DOMParser. */
export function buildAssets(html: string, parser?: Pick<DOMParser, 'parseFromString'>): string[];
/** The same list from a live document; read it at start-up. */
export function pageAssets(doc?: Document): string[];
export interface WatchForUpdateOptions {
  /** The product name in the notice, e.g. "KyIdentity". */
  name?: string;
  /** The entry document to re-fetch; default "/". */
  url?: string;
  /** Minimum milliseconds between checks; default five minutes. */
  interval?: number;
  doc?: Document;
  fetch?: typeof globalThis.fetch;
}
/** Offers a reload when the served build differs from the loaded one. Returns a cleanup. */
export function watchForUpdate(options?: WatchForUpdateOptions): () => void;
