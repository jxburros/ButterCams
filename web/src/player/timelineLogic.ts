// Decisions of the recordings player that need no DOM: how much to buffer,
// when to switch to keyframe-only playback, how a day is laid out.

/** Playback speeds offered (spec 16.5). */
export const SPEEDS = [0.5, 1, 2, 4, 8, 16] as const;
export type Speed = (typeof SPEEDS)[number];

/** Seconds of video kept ahead of and behind the playhead at 1×. */
export const BUFFER_AHEAD_S = 30;
export const BUFFER_BEHIND_S = 30;
/** The next file's index is fetched when the playhead is this close to it. */
export const PREFETCH_INDEX_S = 60;
/** Every frame is played up to this speed; above it only if the browser keeps up. */
export const FULL_FRAME_SPEED = 4;
/** More than this share of dropped frames over the window switches to keyframes. */
export const DROP_SHARE_LIMIT = 0.25;
export const DROP_WINDOW_MS = 2000;

/** How far ahead to buffer at a speed: the same wall time of playback, capped. */
export function bufferAhead(rate: number): number {
  return Math.min(BUFFER_AHEAD_S * Math.max(1, rate), 240);
}

/** Whether more video should be fetched now. */
export function needsMore(currentTime: number, bufferedEnd: number, rate: number): boolean {
  return bufferedEnd - currentTime < bufferAhead(rate);
}

/**
 * The range of old video to remove, or `null` when there is nothing worth
 * removing yet (removals are batched so the buffer is not trimmed every tick).
 */
export function trimRange(bufferedStart: number, currentTime: number): [number, number] | null {
  const keepFrom = currentTime - BUFFER_BEHIND_S;
  if (keepFrom - bufferedStart < 10) return null;
  return [bufferedStart, keepFrom];
}

/** Audio plays at 0.5× to 2× only. */
export function audioAllowed(rate: number): boolean {
  return rate >= 0.5 && rate <= 2;
}

/**
 * Whether playback must use keyframes only: above 4× when the browser drops
 * more than a quarter of the frames. `current` is the mode in use; once in
 * keyframe mode at a speed it stays there until the speed is lowered.
 */
export function wantKeyframes(rate: number, current: boolean, droppedInWindow: number, totalInWindow: number): boolean {
  if (rate <= FULL_FRAME_SPEED) return false;
  if (current) return true;
  if (totalInWindow <= 0) return false;
  return droppedInWindow / totalInWindow > DROP_SHARE_LIMIT;
}

/** A sample of the browser's frame counters. */
export interface QualitySample {
  atMs: number;
  dropped: number;
  total: number;
}

/**
 * Keeps the samples of the last window and returns the dropped and total
 * frame counts over it. The window must be nearly full before it counts.
 */
export function dropWindow(samples: QualitySample[], next: QualitySample): { dropped: number; total: number; full: boolean } {
  samples.push(next);
  while (samples.length > 2 && (samples[1]?.atMs ?? 0) <= next.atMs - DROP_WINDOW_MS) samples.shift();
  const first = samples[0] ?? next;
  return {
    dropped: next.dropped - first.dropped,
    total: next.total - first.total,
    full: next.atMs - first.atMs >= DROP_WINDOW_MS * 0.9,
  };
}

// ------------------------------------------------------------- day layout

/** YYYY-MM-DD → the day's first and last instant in the browser's time zone. */
export function dayBounds(date: string): { startMs: number; endMs: number } | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!m) return null;
  const [y, mo, d] = [Number(m[1]), Number(m[2]), Number(m[3])];
  const start = new Date(y, mo - 1, d);
  if (Number.isNaN(start.getTime()) || start.getMonth() !== mo - 1) return null;
  // Local midnight to the next local midnight: 23 or 25 hours on the days the
  // clocks change, never an assumed 24.
  return { startMs: start.getTime(), endMs: new Date(y, mo - 1, d + 1).getTime() };
}

/** The local date (YYYY-MM-DD) of an instant. */
export function dateOf(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** The date `days` after a YYYY-MM-DD date. */
export function shiftDate(date: string, days: number): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!m) return date;
  return dateOf(new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + days, 12).getTime());
}

const TICK_STEPS_MS = [
  1000, 5000, 10_000, 30_000, 60_000, 5 * 60_000, 10 * 60_000, 30 * 60_000, 3_600_000, 2 * 3_600_000, 3 * 3_600_000, 6 * 3_600_000,
];

/** The distance between axis labels for a visible span, aiming at `target` labels. */
export function tickStep(visibleMs: number, target: number): number {
  for (const step of TICK_STEPS_MS) {
    if (visibleMs / step <= target) return step;
  }
  return TICK_STEPS_MS[TICK_STEPS_MS.length - 1] ?? 3_600_000;
}

export interface Tick {
  ms: number;
  label: string;
}

/**
 * Axis labels for the visible part of a day. Ticks are real instants counted
 * from the day's first instant, labelled with the local clock, so a 25-hour
 * day shows the repeated hour twice and a 23-hour day skips the missing one.
 */
export function ticks(dayStartMs: number, viewStartMs: number, viewEndMs: number, target: number): Tick[] {
  const step = tickStep(viewEndMs - viewStartMs, target);
  const first = dayStartMs + Math.ceil((viewStartMs - dayStartMs) / step) * step;
  const out: Tick[] = [];
  for (let ms = first; ms <= viewEndMs && out.length < 200; ms += step) {
    const d = new Date(ms);
    const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
    out.push({ ms, label: step < 60_000 ? `${hm}:${String(d.getSeconds()).padStart(2, '0')}` : hm });
  }
  return out;
}

/** Shortest and longest visible span of the timeline. */
export const MIN_VIEW_MS = 60_000;

/** Zooms a view window around a fixed instant, staying inside the day. */
export function zoomView(
  view: { startMs: number; endMs: number },
  day: { startMs: number; endMs: number },
  factor: number,
  aroundMs: number,
): { startMs: number; endMs: number } {
  const dayLen = day.endMs - day.startMs;
  const len = Math.min(dayLen, Math.max(MIN_VIEW_MS, (view.endMs - view.startMs) * factor));
  const share = (aroundMs - view.startMs) / Math.max(1, view.endMs - view.startMs);
  return panView({ startMs: aroundMs - share * len, endMs: aroundMs - share * len + len }, day, 0);
}

/** Moves a view window, staying inside the day. */
export function panView(
  view: { startMs: number; endMs: number },
  day: { startMs: number; endMs: number },
  byMs: number,
): { startMs: number; endMs: number } {
  const len = Math.min(view.endMs - view.startMs, day.endMs - day.startMs);
  let start = view.startMs + byMs;
  if (start < day.startMs) start = day.startMs;
  if (start + len > day.endMs) start = day.endMs - len;
  return { startMs: start, endMs: start + len };
}

/** The UTC offset written in an API timestamp, in minutes, or `null`. */
export function offsetOf(ts: string): number | null {
  const m = /(Z|[+-]\d{2}:\d{2})$/.exec(ts);
  if (!m || m[1] === undefined) return null;
  if (m[1] === 'Z') return 0;
  const sign = m[1].startsWith('-') ? -1 : 1;
  return sign * (Number(m[1].slice(1, 3)) * 60 + Number(m[1].slice(4, 6)));
}

/** "UTC−06:00" */
export function formatOffset(minutes: number): string {
  const sign = minutes < 0 ? '−' : '+';
  const abs = Math.abs(minutes);
  return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
}
