// Dates rendered on the server and again in the browser must come out as the
// same text, or React's hydration fails. The browser's own locale and time zone
// would differ from the server's, so these pin both.

const day = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" });
const dayTime = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
  timeZone: "UTC",
});

/** "26 Nov 2026" */
export const formatDay = (iso: string) => day.format(new Date(iso));

/** "26 Nov, 11:25 UTC" */
export const formatDayTime = (iso: string) => `${dayTime.format(new Date(iso))} UTC`;
