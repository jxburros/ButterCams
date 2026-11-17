// The decisions of the live player, kept free of the DOM so they can be
// tested: reconnect delays, live edge control, buffer trimming, message
// parsing and which stream a tile uses (spec 16.4).

import type { LiveMessage, StreamKind } from '../api/types';

/** Delays between reconnect attempts; the last one repeats. */
export const BACKOFF_MS = [1000, 2000, 5000, 10000] as const;

/** The wait before reconnect attempt number `attempt` (0 = the first retry). */
export function backoffDelay(attempt: number): number {
  const index = Math.min(Math.max(0, Math.floor(attempt)), BACKOFF_MS.length - 1);
  return BACKOFF_MS[index] ?? 10000;
}

export const EDGE = {
  /** More than this far behind the newest frame: play slightly faster. */
  speedUpAfterSec: 1,
  /** More than this far behind: jump. */
  jumpAfterSec: 3,
  /** Where a jump lands, measured back from the newest frame. */
  jumpToSec: 0.2,
  /** Back to normal speed once this close again (hysteresis). */
  normalBelowSec: 0.5,
  catchUpRate: 1.1,
  /** Buffered video older than this is removed … */
  keepBehindSec: 10,
  /** … once this much has piled up, so removal is not done on every tick. */
  trimAfterSec: 14,
} as const;

export type EdgeAction =
  | { kind: 'rate'; rate: number }
  | { kind: 'seek'; to: number };

/**
 * Keeps the player at the live edge: called every 500 ms with the playback
 * position, the end of the buffer and the current playback rate.
 */
export function liveEdgeAction(currentTime: number, bufferedEnd: number, currentRate: number): EdgeAction {
  const behind = bufferedEnd - currentTime;
  if (behind > EDGE.jumpAfterSec) {
    return { kind: 'seek', to: Math.max(0, bufferedEnd - EDGE.jumpToSec) };
  }
  if (behind > EDGE.speedUpAfterSec) return { kind: 'rate', rate: EDGE.catchUpRate };
  if (currentRate !== 1 && behind > EDGE.normalBelowSec) return { kind: 'rate', rate: currentRate };
  return { kind: 'rate', rate: 1 };
}

/**
 * Where to continue when playback stands at a hole in the buffer: data of a
 * later range arrived but the browser will not play across the hole (a
 * session switch at the top of the hour can leave one in the audio). Returns
 * the start of the next range, or `null` when there is no hole to skip.
 * `ranges` are the buffered ranges in order.
 */
export function holeSkip(ranges: Array<[number, number]>, currentTime: number): number | null {
  for (let i = 0; i + 1 < ranges.length; i++) {
    const [, end] = ranges[i]!;
    const [nextStart] = ranges[i + 1]!;
    // At (or just before) the end of a range, or inside the hole after it.
    if (currentTime >= end - HOLE_EPSILON_SEC && currentTime < nextStart) {
      return nextStart + 0.01;
    }
  }
  return null;
}

/** How close to the end of a buffered range counts as standing at its hole. */
export const HOLE_EPSILON_SEC = 0.15;

/**
 * The range of old video to remove from the buffer, or `null` when there is
 * not enough of it yet.
 */
export function trimRange(bufferedStart: number, currentTime: number): [number, number] | null {
  if (currentTime - bufferedStart < EDGE.trimAfterSec) return null;
  const end = currentTime - EDGE.keepBehindSec;
  return end > bufferedStart ? [bufferedStart, end] : null;
}

/** Reads a text message of the live WebSocket; `null` for anything unknown. */
export function parseLiveMessage(text: string): LiveMessage | null {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof value !== 'object' || value === null) return null;
  const v = value as Record<string, unknown>;
  if (v.type === 'init' && typeof v.mime === 'string' && v.mime !== '') {
    return {
      type: 'init',
      mime: v.mime,
      width: typeof v.width === 'number' ? v.width : 0,
      height: typeof v.height === 'number' ? v.height : 0,
    };
  }
  if (v.type === 'state') {
    return { type: 'state', state: 'waiting', message: typeof v.message === 'string' ? v.message : '' };
  }
  return null;
}

export interface StreamChoiceInput {
  /** A grid tile prefers the sub stream; single view prefers the main stream. */
  mode: 'tile' | 'single';
  hasSub: boolean;
  /** `live.mainStream` of the system settings. */
  mainEnabled: boolean;
  /** Streams this browser turned out not to be able to play (H.265). */
  unsupported: ReadonlySet<StreamKind>;
}

/** Which stream to open, or `null` when none of the camera's streams can be played here. */
export function pickStream(input: StreamChoiceInput): StreamKind | null {
  const usable = (stream: StreamKind) =>
    !input.unsupported.has(stream) && (stream === 'sub' ? input.hasSub : input.mainEnabled);
  const order: StreamKind[] = input.mode === 'tile' ? ['sub', 'main'] : ['main', 'sub'];
  for (const stream of order) {
    if (usable(stream)) return stream;
  }
  return null;
}

/** "H.265" or "H.264" … from a MIME type with codecs, for messages. */
export function codecLabel(mime: string): string {
  const m = /codecs="?([^";]+)/.exec(mime);
  const first = (m?.[1] ?? '').split(',')[0]?.trim().toLowerCase() ?? '';
  if (first.startsWith('hvc1') || first.startsWith('hev1')) return 'H.265';
  if (first.startsWith('avc1') || first.startsWith('avc3')) return 'H.264';
  return first === '' ? 'this video format' : first;
}

/** What a tile says when the browser cannot play the camera's video. */
export function unsupportedMessage(mime: string): string {
  const codec = codecLabel(mime);
  if (codec === 'H.265') {
    return 'This browser cannot play H.265 video. Safari, and Chrome or Edge on a device with H.265 hardware support, can. Or set the camera’s sub stream to H.264.';
  }
  return `This browser cannot play ${codec}.`;
}

/** The plain HTTP address of a live WebSocket URL, for reading the error behind a refused connection. */
export function probeUrl(wsUrl: string): string {
  return wsUrl.replace(/^ws(s?):/, 'http$1:');
}
