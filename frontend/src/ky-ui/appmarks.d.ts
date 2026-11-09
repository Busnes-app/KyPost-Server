export declare const appMarks: Readonly<Record<string, string>>;
export declare function appMark(id: string): string;
/** The entry's URL resolved against `base` (default: the page) when it is http(s), else null. */
export declare function appHref(url: unknown, base?: string): string | null;
