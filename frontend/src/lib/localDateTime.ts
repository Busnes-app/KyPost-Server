/** A Date as a datetime-local input value, in local time to the minute. */
export function localDateTimeValue(d: Date): string {
  return new Date(d.getTime() - d.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}
