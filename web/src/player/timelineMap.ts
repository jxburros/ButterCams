// The timeline map of the recordings player (spec 16.5, docs/V2_PLAN.md M2
// step 4). Everything here is pure: no DOM, no network.
//
// The browser plays hour files through one MediaSource. Each file gets a
// place on the player's own media timeline:
//
//   - a file that follows the previous one without a gap is placed exactly at
//     the previous file's end, by media duration, so the SourceBuffer sees no
//     hole and playback crosses the seam invisibly;
//   - a file after a real gap is placed after a deliberate jump, which the
//     player skips while saying what was skipped.
//
// The wall-clock time shown is mapped per file.

import type { PlayChunk, PlayIndex } from '../api/types';

/** A recorded span as the timeline lists it, with times in epoch milliseconds. */
export interface Span {
  id: number;
  startMs: number;
  /** `null` while the file is being recorded. */
  endMs: number | null;
  gapBefore: boolean;
  open: boolean;
  videoCodec: string;
  fileName: string;
}

/** A file placed on the player's media timeline. Media times are in seconds. */
export interface Placed {
  id: number;
  /** Wall-clock time of the file's first frame, epoch milliseconds. */
  wallStartMs: number;
  mediaStart: number;
  /** Length of the video in the file, seconds. Grows while the file is open. */
  duration: number;
  /** `true` when the file was placed after a jump: it does not continue the previous one. */
  afterGap: boolean;
}

/** A hole in the recording between two placed files. */
export interface PlacedGap {
  mediaFrom: number;
  mediaTo: number;
  wallFromMs: number;
  wallToMs: number;
}

/** Files closer together than this are one continuous recording. */
export const JOIN_TOLERANCE_MS = 1000;
/** Media seconds left empty between two files that are separated by a real gap. */
export const GAP_JUMP_S = 5;
/** Media time of the first file placed after a seek. */
export const ANCHOR_S = 10;

/** Whether `next` continues `prev` without a gap. */
export function continues(prev: Span, next: Span): boolean {
  if (next.gapBefore || prev.endMs === null) return false;
  return Math.abs(next.startMs - prev.endMs) <= JOIN_TOLERANCE_MS;
}

/** Places the first file of a chain (after a seek). */
export function anchor(span: Span, index: Pick<PlayIndex, 'start' | 'durationMs'>): Placed {
  return {
    id: span.id,
    wallStartMs: wallStartOf(span, index),
    mediaStart: ANCHOR_S,
    duration: index.durationMs / 1000,
    afterGap: true,
  };
}

function wallStartOf(span: Span, index: Pick<PlayIndex, 'start'>): number {
  const parsed = Date.parse(index.start);
  return Number.isNaN(parsed) ? span.startMs : parsed;
}

/**
 * Places the file that plays after `prev`. `prev.duration` must be final (the
 * previous file is closed and its index complete).
 */
export function placeNext(
  prev: Placed,
  prevSpan: Span,
  nextSpan: Span,
  nextIndex: Pick<PlayIndex, 'start' | 'durationMs'>,
): { placed: Placed; gap: PlacedGap | null } {
  const prevEnd = prev.mediaStart + prev.duration;
  const wallStartMs = wallStartOf(nextSpan, nextIndex);
  if (continues(prevSpan, nextSpan)) {
    return {
      placed: { id: nextSpan.id, wallStartMs, mediaStart: prevEnd, duration: nextIndex.durationMs / 1000, afterGap: false },
      gap: null,
    };
  }
  const mediaStart = prevEnd + GAP_JUMP_S;
  return {
    placed: { id: nextSpan.id, wallStartMs, mediaStart, duration: nextIndex.durationMs / 1000, afterGap: true },
    gap: {
      mediaFrom: prevEnd,
      mediaTo: mediaStart,
      wallFromMs: prev.wallStartMs + prev.duration * 1000,
      wallToMs: wallStartMs,
    },
  };
}

/** The placed file that holds a media time, or the nearest one before it. */
export function placedAt(chain: readonly Placed[], media: number): Placed | null {
  let found: Placed | null = null;
  for (const p of chain) {
    if (media >= p.mediaStart - 1e-6) found = p;
    else break;
  }
  return found ?? chain[0] ?? null;
}

/** Media time → wall-clock time (epoch ms). Inside a gap it is the end of the file before it. */
export function mediaToWall(chain: readonly Placed[], media: number): number | null {
  const p = placedAt(chain, media);
  if (!p) return null;
  const inside = Math.min(Math.max(media - p.mediaStart, 0), p.duration);
  return p.wallStartMs + inside * 1000;
}

/** Wall-clock time → media time, or `null` when no placed file covers it. */
export function wallToMedia(chain: readonly Placed[], wallMs: number): number | null {
  for (const p of chain) {
    const offset = (wallMs - p.wallStartMs) / 1000;
    if (offset >= 0 && offset <= p.duration) return p.mediaStart + offset;
  }
  return null;
}

/** Where a requested wall-clock time lands among the recorded spans. */
export type SeekTarget =
  | { kind: 'inside'; span: Span; offsetMs: number }
  | { kind: 'skipped'; span: Span; offsetMs: 0; skippedFromMs: number; skippedToMs: number }
  | { kind: 'none'; lastEndMs: number | null };

/**
 * Finds the span to play for a wall-clock time. A time inside a gap goes to
 * the next span, and the answer says what was skipped. `nowMs` stands in for
 * the end of the span still being recorded.
 */
export function seekTarget(spans: readonly Span[], wallMs: number, nowMs: number): SeekTarget {
  let lastEnd: number | null = null;
  for (const span of spans) {
    const end = span.endMs ?? Math.max(nowMs, span.startMs);
    if (wallMs < span.startMs) {
      // The whole hole is reported, not only the part after the requested moment.
      return { kind: 'skipped', span, offsetMs: 0, skippedFromMs: lastEnd ?? wallMs, skippedToMs: span.startMs };
    }
    if (wallMs < end) return { kind: 'inside', span, offsetMs: wallMs - span.startMs };
    lastEnd = end;
  }
  return { kind: 'none', lastEndMs: lastEnd };
}

/** The span that plays after the given one, by start time. */
export function nextSpan(spans: readonly Span[], id: number): Span | null {
  const i = spans.findIndex((s) => s.id === id);
  return i >= 0 ? (spans[i + 1] ?? null) : null;
}

/** Whether a wall-clock time has a recording. */
export function recordedAt(spans: readonly Span[], wallMs: number, nowMs: number): boolean {
  return seekTarget(spans, wallMs, nowMs).kind === 'inside';
}

/**
 * Index of the chunk that holds a time (ms from the start of the file): the
 * last chunk starting at or before it. Times before the first chunk give 0,
 * times after the last chunk give the last chunk; -1 only for an empty list.
 */
export function chunkAt(chunks: readonly PlayChunk[], ms: number): number {
  let lo = 0;
  let hi = chunks.length - 1;
  let found = chunks.length > 0 ? 0 : -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const chunk = chunks[mid];
    if (chunk !== undefined && chunk[0] <= ms) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
}

/**
 * The run of consecutive chunks to fetch in one range request, starting at
 * `from`: as many as fit in the byte and time limits, at least one.
 */
export function chunkRun(
  chunks: readonly PlayChunk[],
  from: number,
  maxBytes: number,
  maxMs: number,
): { count: number; offset: number; size: number } | null {
  const first = chunks[from];
  if (first === undefined) return null;
  let count = 1;
  let size = first[3];
  let ms = first[1];
  for (let i = from + 1; i < chunks.length; i++) {
    const chunk = chunks[i];
    const prev = chunks[i - 1];
    if (chunk === undefined || prev === undefined) break;
    if (chunk[2] !== prev[2] + prev[3]) break; // not adjacent in the file
    if (size + chunk[3] > maxBytes || ms + chunk[1] > maxMs) break;
    size += chunk[3];
    ms += chunk[1];
    count++;
  }
  return { count, offset: first[2], size };
}

function two(n: number): string {
  return String(n).padStart(2, '0');
}

/** HH:MM:SS in the browser's time zone. */
export function clockTime(ms: number): string {
  const d = new Date(ms);
  return `${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`;
}

/** "No recording 09:12:04 – 09:14:31", with dates when the gap crosses midnight. */
export function gapMessage(fromMs: number, toMs: number): string {
  const a = new Date(fromMs);
  const b = new Date(toMs);
  const sameDay = a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (sameDay) return `No recording ${clockTime(fromMs)} – ${clockTime(toMs)}`;
  const day = (d: Date) => d.toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
  return `No recording ${day(a)} ${clockTime(fromMs)} – ${day(b)} ${clockTime(toMs)}`;
}
