import { beforeAll, describe, expect, it } from 'vitest';

// The clock-change days below are those of this zone. Set before any Date is
// formatted in this file.
(globalThis as unknown as { process: { env: Record<string, string> } }).process.env.TZ = 'America/Chicago';

import {
  audioAllowed,
  bufferAhead,
  dateOf,
  dayBounds,
  dropWindow,
  formatOffset,
  MIN_VIEW_MS,
  needsMore,
  offsetOf,
  panView,
  shiftDate,
  ticks,
  tickStep,
  trimRange,
  wantKeyframes,
  zoomView,
} from './timelineLogic';
import type { QualitySample } from './timelineLogic';
import { LatestOnly, LruCache, THUMB_BUCKET_MS, thumbKey } from './thumbnails';

const H = 3_600_000;

describe('buffer window', () => {
  it('keeps about 30 s ahead, more at higher speeds', () => {
    expect(needsMore(100, 120, 1)).toBe(true);
    expect(needsMore(100, 131, 1)).toBe(false);
    expect(needsMore(100, 131, 4)).toBe(true);
    expect(bufferAhead(0.5)).toBe(30);
    expect(bufferAhead(16)).toBe(240);
  });

  it('removes video more than 30 s behind, in batches', () => {
    expect(trimRange(0, 20)).toBeNull();
    expect(trimRange(0, 35)).toBeNull(); // only 5 s to remove: not yet
    expect(trimRange(0, 45)).toEqual([0, 15]);
    expect(trimRange(100, 200)).toEqual([100, 170]);
  });
});

describe('speeds', () => {
  it('plays audio at 0.5× to 2× only', () => {
    expect([0.5, 1, 2, 4, 8, 16].map(audioAllowed)).toEqual([true, true, true, false, false, false]);
  });

  it('switches to keyframes above 4× only when the browser drops frames', () => {
    expect(wantKeyframes(4, false, 90, 100)).toBe(false); // up to 4× every frame is played
    expect(wantKeyframes(8, false, 10, 100)).toBe(false); // the browser keeps up
    expect(wantKeyframes(8, false, 25, 100)).toBe(false);
    expect(wantKeyframes(8, false, 26, 100)).toBe(true);
    expect(wantKeyframes(16, false, 0, 0)).toBe(false); // nothing measured yet
    expect(wantKeyframes(16, true, 0, 100)).toBe(true); // stays until the speed is lowered
    expect(wantKeyframes(2, true, 0, 100)).toBe(false);
  });

  it('measures dropped frames over a two-second window', () => {
    const samples: QualitySample[] = [];
    let w = dropWindow(samples, { atMs: 0, dropped: 0, total: 0 });
    expect(w.full).toBe(false);
    for (let t = 250; t <= 2000; t += 250) w = dropWindow(samples, { atMs: t, dropped: t / 25, total: t / 5 });
    expect(w).toEqual({ dropped: 80, total: 400, full: true });
    // Old samples fall out: the window does not grow.
    for (let t = 2250; t <= 6000; t += 250) w = dropWindow(samples, { atMs: t, dropped: 80, total: t / 5 });
    expect(w.dropped).toBe(0);
    expect(w.total).toBe(400);
    expect(samples.length).toBeLessThan(12);
  });
});

describe('day layout', () => {
  beforeAll(() => {
    // Guard: the tests below are only meaningful in a zone with clock changes.
    expect(new Date(2026, 0, 1).getTimezoneOffset()).toBe(360);
  });

  it('gives an ordinary day 24 hours', () => {
    const day = dayBounds('2026-11-14');
    expect(day).not.toBeNull();
    expect(((day?.endMs ?? 0) - (day?.startMs ?? 0)) / H).toBe(24);
    expect(new Date(day?.startMs ?? 0).getHours()).toBe(0);
  });

  it('gives the spring clock-change day 23 hours and the autumn one 25', () => {
    const spring = dayBounds('2026-03-08');
    const autumn = dayBounds('2026-11-01');
    expect(((spring?.endMs ?? 0) - (spring?.startMs ?? 0)) / H).toBe(23);
    expect(((autumn?.endMs ?? 0) - (autumn?.startMs ?? 0)) / H).toBe(25);
  });

  it('labels the repeated hour twice and skips the missing one', () => {
    const autumn = dayBounds('2026-11-01');
    const spring = dayBounds('2026-03-08');
    if (!autumn || !spring) throw new Error('no day');
    const a = ticks(autumn.startMs, autumn.startMs, autumn.endMs, 30).map((t) => t.label);
    expect(a.slice(0, 4)).toEqual(['00:00', '01:00', '01:00', '02:00']);
    expect(a).toHaveLength(26); // 25 hours and the closing midnight
    const s = ticks(spring.startMs, spring.startMs, spring.endMs, 30).map((t) => t.label);
    expect(s.slice(0, 4)).toEqual(['00:00', '01:00', '03:00', '04:00']);
    expect(s).toHaveLength(24);
  });

  it('rejects dates that do not exist', () => {
    expect(dayBounds('2026-02-30')).toBeNull();
    expect(dayBounds('yesterday')).toBeNull();
  });

  it('moves between dates', () => {
    expect(shiftDate('2026-03-08', 1)).toBe('2026-03-09');
    expect(shiftDate('2026-11-01', -1)).toBe('2026-10-31');
    expect(shiftDate('2026-12-31', 1)).toBe('2027-01-01');
    expect(dateOf(new Date(2026, 10, 14, 23, 59).getTime())).toBe('2026-11-14');
  });

  it('chooses label distances for the zoom level', () => {
    expect(tickStep(24 * H, 12)).toBe(2 * H);
    expect(tickStep(H, 12)).toBe(5 * 60_000);
    expect(tickStep(60_000, 12)).toBe(5000);
    const day = dayBounds('2026-11-14');
    if (!day) throw new Error('no day');
    const t = ticks(day.startMs, day.startMs + 9 * H + 30_000, day.startMs + 9 * H + 90_000, 12);
    expect(t[0]?.label).toBe('09:00:30');
    expect(t[1]?.label).toBe('09:00:35');
  });

  it('zooms around a point between one minute and the whole day, and pans inside the day', () => {
    const day = { startMs: 0, endMs: 24 * H };
    let view = zoomView(day, day, 0.5, 6 * H);
    expect(view).toEqual({ startMs: 3 * H, endMs: 15 * H }); // 06:00 stays a quarter in
    for (let i = 0; i < 40; i++) view = zoomView(view, day, 0.5, 6 * H);
    expect(view.endMs - view.startMs).toBe(MIN_VIEW_MS);
    expect(view.startMs).toBeLessThanOrEqual(6 * H);
    expect(view.endMs).toBeGreaterThanOrEqual(6 * H);
    for (let i = 0; i < 40; i++) view = zoomView(view, day, 2, 6 * H);
    expect(view).toEqual(day);
    expect(panView({ startMs: H, endMs: 2 * H }, day, -5 * H)).toEqual({ startMs: 0, endMs: H });
    expect(panView({ startMs: H, endMs: 2 * H }, day, 50 * H)).toEqual({ startMs: 23 * H, endMs: 24 * H });
  });

  it('reads the offset the server wrote in a timestamp', () => {
    expect(offsetOf('2026-11-14T09:00:00.212-06:00')).toBe(-360);
    expect(offsetOf('2026-11-14T09:00:00Z')).toBe(0);
    expect(offsetOf('2026-11-14T09:00:00+05:30')).toBe(330);
    expect(offsetOf('nonsense')).toBeNull();
    expect(formatOffset(-360)).toBe('UTC−06:00');
    expect(formatOffset(330)).toBe('UTC+05:30');
  });
});

describe('thumbnail cache', () => {
  it('keeps the most recently used pictures up to the limit', () => {
    const cache = new LruCache<string>(3);
    cache.set('a', '1');
    cache.set('b', '2');
    cache.set('c', '3');
    expect(cache.get('a')).toBe('1'); // a is now the newest
    cache.set('d', '4');
    expect(cache.size).toBe(3);
    expect(cache.get('b')).toBeUndefined();
    expect(cache.get('a')).toBe('1');
    expect(cache.get('d')).toBe('4');
  });

  it('shares one picture per bucket of a file', () => {
    expect(thumbKey(7, 0)).toEqual({ key: '7:0', tMs: 0 });
    expect(thumbKey(7, THUMB_BUCKET_MS - 1).key).toBe('7:0');
    expect(thumbKey(7, THUMB_BUCKET_MS).key).toBe(`7:${THUMB_BUCKET_MS}`);
    expect(thumbKey(8, 0).key).not.toBe(thumbKey(7, 0).key);
    expect(thumbKey(7, -50).tMs).toBe(0);
  });

  it('runs one request at a time and only the latest of those that waited', async () => {
    const started: number[] = [];
    const release: (() => void)[] = [];
    const queue = new LatestOnly<number>((job) => {
      started.push(job);
      return new Promise<void>((resolve) => release.push(resolve));
    });
    queue.request(1);
    queue.request(2);
    queue.request(3);
    queue.request(4);
    expect(started).toEqual([1]);
    release[0]?.();
    await new Promise((r) => setTimeout(r, 0));
    expect(started).toEqual([1, 4]); // 2 and 3 were overtaken
    release[1]?.();
    await new Promise((r) => setTimeout(r, 0));
    queue.request(5);
    expect(started).toEqual([1, 4, 5]);
  });
});
