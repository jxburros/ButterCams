import { describe, expect, it } from 'vitest';
import type { PlayChunk } from '../api/types';
import {
  anchor,
  ANCHOR_S,
  chunkAt,
  chunkRun,
  firstFrameMs,
  seamKeep,
  continues,
  GAP_JUMP_S,
  gapMessage,
  mediaToWall,
  nextSpan,
  placedAt,
  placeNext,
  recordedAt,
  seekTarget,
  wallToMedia,
} from './timelineMap';
import type { Placed, Span } from './timelineMap';

const H = 3_600_000;
const T0 = Date.parse('2026-11-14T09:00:00.000-06:00');

function span(id: number, startMs: number, endMs: number | null, gapBefore = false): Span {
  return { id, startMs, endMs, gapBefore, open: endMs === null, videoCodec: 'h264', fileName: `f${id}.mp4` };
}

function idx(startMs: number, durationMs: number) {
  return { start: new Date(startMs).toISOString(), durationMs };
}

describe('timeline map', () => {
  // Three hour files in a row, then a real gap, then one more.
  const a = span(1, T0, T0 + H + 37); // media is a little longer than the clock hour
  const b = span(2, T0 + H + 40, T0 + 2 * H + 12);
  const c = span(3, T0 + 2 * H + 15, T0 + 2 * H + 30 * 60_000);
  const d = span(4, T0 + 3 * H, T0 + 4 * H, true);

  it('joins files without a gap end to start by exact media duration', () => {
    expect(continues(a, b)).toBe(true);
    const pa = anchor(a, idx(a.startMs, 3_600_033));
    expect(pa.mediaStart).toBe(ANCHOR_S);
    const { placed: pb, gap } = placeNext(pa, b, idx(b.startMs, 3_599_970));
    expect(gap).toBeNull();
    // Not the wall clock difference (3600.040 s) but the media duration of the first file.
    expect(pb.mediaStart).toBeCloseTo(ANCHOR_S + 3600.033, 9);
    expect(pb.afterGap).toBe(false);
    const { placed: pc, gap: gap2 } = placeNext(pb, c, idx(c.startMs, 1_800_000));
    expect(gap2).toBeNull();
    expect(pc.mediaStart).toBeCloseTo(pb.mediaStart + 3599.97, 9);
  });

  it('places a file after a real gap behind a jump and reports the gap', () => {
    expect(continues(c, d)).toBe(false);
    const pc: Placed = { id: 3, wallStartMs: c.startMs, mediaStart: 100, duration: 1800, afterGap: false };
    const { placed, gap } = placeNext(pc, d, idx(d.startMs, 3_600_000));
    expect(placed.mediaStart).toBe(1900 + GAP_JUMP_S);
    expect(placed.afterGap).toBe(true);
    expect(gap).toEqual({ mediaFrom: 1900, mediaTo: 1900 + GAP_JUMP_S, wallFromMs: c.startMs + 1_800_000, wallToMs: d.startMs });
  });

  it('treats gapBefore, a late start and an open predecessor as gaps', () => {
    expect(continues(a, { ...b, gapBefore: true })).toBe(false);
    expect(continues(a, { ...b, startMs: (a.endMs ?? 0) + 1001 })).toBe(false);
    expect(continues(a, { ...b, startMs: (a.endMs ?? 0) + 1000 })).toBe(true);
    expect(continues(span(9, T0, null), b)).toBe(false);
  });

  it('maps media time to wall time per file and back', () => {
    const chain: Placed[] = [
      { id: 1, wallStartMs: T0, mediaStart: 10, duration: 3600.033, afterGap: true },
      { id: 2, wallStartMs: T0 + H + 40, mediaStart: 3610.033, duration: 3599.97, afterGap: false },
      { id: 4, wallStartMs: T0 + 3 * H, mediaStart: 7215.003, duration: 3600, afterGap: true },
    ];
    expect(mediaToWall(chain, 10)).toBe(T0);
    expect(mediaToWall(chain, 70)).toBe(T0 + 60_000);
    // Just past the seam the clock is the second file's own.
    expect(mediaToWall(chain, 3610.033 + 5)).toBeCloseTo(T0 + H + 40 + 5000, 3);
    // Inside the jump the clock stays at the end of the file before it.
    expect(mediaToWall(chain, 7212)).toBeCloseTo(T0 + H + 40 + 3_599_970, 3);
    expect(mediaToWall(chain, 7215.003 + 1)).toBeCloseTo(T0 + 3 * H + 1000, 3);
    expect(mediaToWall([], 5)).toBeNull();

    expect(wallToMedia(chain, T0 + 60_000)).toBeCloseTo(70, 9);
    expect(wallToMedia(chain, T0 + 3 * H + 1000)).toBeCloseTo(7216.003, 6);
    expect(wallToMedia(chain, T0 + 2 * H + 45 * 60_000)).toBeNull(); // in the gap
    expect(wallToMedia(chain, T0 - 1)).toBeNull();
    expect(placedAt(chain, 5000)?.id).toBe(2);
    expect(placedAt(chain, 0)?.id).toBe(1);
  });

  it('finds what to play for a wall-clock time', () => {
    const spans = [a, b, c, d];
    const now = T0 + 10 * H;
    const inside = seekTarget(spans, T0 + H + 60_000, now);
    expect(inside).toMatchObject({ kind: 'inside', span: b, offsetMs: 60_000 - 40 });
    // Into the gap between c and d: go to d and say what was skipped.
    const at = T0 + 2 * H + 40 * 60_000;
    expect(seekTarget(spans, at, now)).toMatchObject({ kind: 'skipped', span: d, skippedFromMs: c.endMs, skippedToMs: d.startMs });
    // Before the first recording.
    expect(seekTarget(spans, T0 - H, now)).toMatchObject({ kind: 'skipped', span: a, skippedFromMs: T0 - H });
    // After the last one.
    expect(seekTarget(spans, T0 + 5 * H, now)).toEqual({ kind: 'none', lastEndMs: d.endMs });
    expect(seekTarget([], T0, now)).toEqual({ kind: 'none', lastEndMs: null });
    // The file being recorded reaches up to now.
    const open = span(5, T0 + 9 * H, null);
    expect(seekTarget([...spans, open], now - 1000, now)).toMatchObject({ kind: 'inside', span: open });
    expect(seekTarget([...spans, open], now + 1000, now).kind).toBe('none');
    expect(recordedAt(spans, T0 + 1, now)).toBe(true);
    expect(recordedAt(spans, at, now)).toBe(false);
    expect(nextSpan(spans, 2)).toBe(c);
    expect(nextSpan(spans, 4)).toBeNull();
    expect(nextSpan(spans, 99)).toBeNull();
  });

  it('describes a gap', () => {
    const from = new Date(2026, 10, 14, 9, 12, 4).getTime();
    const to = new Date(2026, 10, 14, 9, 14, 31).getTime();
    expect(gapMessage(from, to)).toBe('No recording 09:12:04 – 09:14:31');
    expect(gapMessage(from, to + 24 * H)).toMatch(/^No recording .+ 09:12:04 – .+ 09:14:31$/);
  });
});

describe('chunks', () => {
  const chunks: PlayChunk[] = [
    [0, 1000, 900, 5000],
    [1000, 1000, 5900, 4000],
    [2000, 4000, 9900, 20000],
    [6000, 1000, 29900, 3000],
  ];

  it('finds the chunk that holds a time', () => {
    expect(chunkAt(chunks, 0)).toBe(0);
    expect(chunkAt(chunks, 999)).toBe(0);
    expect(chunkAt(chunks, 1000)).toBe(1);
    expect(chunkAt(chunks, 5999)).toBe(2);
    expect(chunkAt(chunks, 6000)).toBe(3);
    expect(chunkAt(chunks, 99_999)).toBe(3);
    expect(chunkAt(chunks, -5)).toBe(0);
    expect(chunkAt([], 0)).toBe(-1);
  });

  it('groups adjacent chunks into one range request within the limits', () => {
    expect(chunkRun(chunks, 0, 1 << 20, 60_000)).toEqual({ count: 4, offset: 900, size: 32000 });
    expect(chunkRun(chunks, 0, 10_000, 60_000)).toEqual({ count: 2, offset: 900, size: 9000 });
    expect(chunkRun(chunks, 1, 1 << 20, 4500)).toEqual({ count: 1, offset: 5900, size: 4000 });
    // A chunk larger than the limit is still fetched, alone.
    expect(chunkRun(chunks, 2, 100, 100)).toEqual({ count: 1, offset: 9900, size: 20000 });
    expect(chunkRun(chunks, 4, 1 << 20, 60_000)).toBeNull();
    // Chunks that are not adjacent in the file are not merged.
    const holed: PlayChunk[] = [
      [0, 1000, 0, 100],
      [1000, 1000, 500, 100],
    ];
    expect(chunkRun(holed, 0, 1 << 20, 60_000)).toEqual({ count: 1, offset: 0, size: 100 });
  });
});

describe('rollover seam', () => {
  // A file of 10 s in 1 s chunks, 15 frames a second.
  const chunks: PlayChunk[] = Array.from({ length: 10 }, (_, i) => [i * 1000, 1000, 1000 + i * 500, 500] as PlayChunk);
  const frame = 1000 / 15;

  it('keeps everything when the files do not overlap', () => {
    expect(seamKeep(chunks, 0, 10_000, 10_000, frame)).toEqual({ keep: 10, keptMs: 10_000 });
    expect(seamKeep(chunks, 0, 10_000, 10_020, frame)).toEqual({ keep: 10, keptMs: 10_000 }); // a small hole
    expect(seamKeep(chunks, 0, 10_000, 9_980, frame)).toEqual({ keep: 10, keptMs: 10_000 }); // under half a frame
  });

  it('leaves out the chunk the new file repeats', () => {
    // The new file starts where the old file's last chunk starts.
    expect(seamKeep(chunks, 0, 10_000, 9_000, frame)).toEqual({ keep: 9, keptMs: 9_000 });
    // ... or within a frame after it.
    expect(seamKeep(chunks, 0, 10_000, 9_050, frame)).toEqual({ keep: 9, keptMs: 9_000 });
    // Two chunks repeated.
    expect(seamKeep(chunks, 0, 10_000, 8_000.4, frame)).toEqual({ keep: 8, keptMs: 8_000 });
    // An overlap inside the last chunk only: nothing starts after the cut.
    expect(seamKeep(chunks, 0, 10_000, 9_500, frame)).toEqual({ keep: 10, keptMs: 10_000 });
  });

  it('never drops a whole file and ignores absurd overlaps', () => {
    expect(seamKeep(chunks.slice(0, 1), 0, 1000, 0, frame).keep).toBe(1);
    expect(seamKeep(chunks, 0, 10_000, 2_000, frame)).toEqual({ keep: 10, keptMs: 10_000 });
  });

  it('joins the new file at the end of what was kept', () => {
    const { keptMs } = seamKeep(chunks, T0, 10_000, T0 + 9_000, frame);
    const prev: Placed = { id: 1, wallStartMs: T0, mediaStart: 10, duration: keptMs / 1000, afterGap: true };
    const { placed, gap } = placeNext(prev, span(2, T0 + 9_000, T0 + 19_000), { start: new Date(T0 + 9_000).toISOString(), durationMs: 10_000 });
    expect(gap).toBeNull();
    expect(placed.mediaStart).toBe(19);
    // A file that does not say when it starts uses the span's start.
    expect(placeNext(prev, span(2, T0 + 9_000, T0 + 19_000), { start: null, durationMs: 10_000 }).placed.wallStartMs).toBe(T0 + 9_000);
  });

  it('reads the duration of the first video frame of a fragment', () => {
    const box = (type: string, ...parts: number[][]) => {
      const body = parts.flat();
      const size = body.length + 8;
      return [size >>> 24, (size >>> 16) & 255, (size >>> 8) & 255, size & 255, ...[...type].map((c) => c.charCodeAt(0)), ...body];
    };
    const u32 = (n: number) => [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255];
    const traf = (track: number, dur: number) =>
      box('traf', box('tfhd', u32(0x020000), u32(track)), box('tfdt', u32(1 << 24), u32(0), u32(0)), box('trun', u32((1 << 24) | 0x301), u32(2), u32(100), u32(dur), u32(10), u32(dur), u32(10)));
    const moof = box('moof', box('mfhd', u32(0), u32(1)), traf(2, 1024), traf(1, 6000));
    const bytes = new Uint8Array([...moof, ...box('mdat', [1, 2, 3])]);
    expect(firstFrameMs(bytes.buffer)).toBeCloseTo(6000 / 90, 6);
    expect(firstFrameMs(new Uint8Array([0, 0, 0, 8, 102, 114, 101, 101]).buffer)).toBe(0);
    expect(firstFrameMs(new ArrayBuffer(3))).toBe(0);
  });
});
