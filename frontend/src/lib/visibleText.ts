// Untrusted text (sender envelopes, remote folder names): control, format
// (bidi, zero-width), line/paragraph separators, blank "letters" and every
// stacked combining mark after the first are shown as code points, never applied.
const hidden = /[\p{Cc}\p{Cf}\u2028\u2029\u034F\u115F\u1160\u17B4\u17B5\u180E\u2800\u3164\uFFA0]|(?<=[\p{Mn}\p{Me}])[\p{Mn}\p{Me}]/gu;

/** visible escapes hidden code points; a run of one collapses to a single counted token. */
export function visible(value: string): string {
  return value.replace(hidden, c => `[U+${c.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}]`)
    .replace(/\[U\+([0-9A-F]+)\](?:\[U\+\1\])+/g, (run, hex: string) => `[U+${hex} \u00D7${run.length / (hex.length + 4)}]`);
}
