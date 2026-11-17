// Thumbnails on hover (spec 16.5): the server returns a tiny, complete MP4
// holding one keyframe; the browser decodes it in a hidden <video>, draws it
// to a small canvas and keeps the picture. The server decodes nothing.

import { api } from '../api/client';

/** Pictures kept in memory, across all cameras. */
export const THUMB_LIMIT = 500;
export const THUMB_WIDTH = 160;
/** Hover positions inside one bucket share a picture. */
export const THUMB_BUCKET_MS = 2000;

/** A least-recently-used cache with a fixed size. */
export class LruCache<V> {
  private readonly map = new Map<string, V>();
  private readonly limit: number;

  constructor(limit: number) {
    this.limit = limit;
  }

  get(key: string): V | undefined {
    const value = this.map.get(key);
    if (value !== undefined) {
      this.map.delete(key); // move to the newest end
      this.map.set(key, value);
    }
    return value;
  }

  set(key: string, value: V): void {
    this.map.delete(key);
    this.map.set(key, value);
    while (this.map.size > this.limit) {
      const oldest = this.map.keys().next();
      if (oldest.done) break;
      this.map.delete(oldest.value);
    }
  }

  get size(): number {
    return this.map.size;
  }
}

/** The cache key and request time for a position in a file. */
export function thumbKey(segmentId: number, offsetMs: number): { key: string; tMs: number } {
  const tMs = Math.max(0, Math.floor(offsetMs / THUMB_BUCKET_MS) * THUMB_BUCKET_MS);
  return { key: `${segmentId}:${tMs}`, tMs };
}

/**
 * Runs one job at a time; while one runs, only the most recent request is
 * remembered ("the latest hover position wins").
 */
export class LatestOnly<T> {
  private running = false;
  private waiting: T | null = null;
  private readonly run: (job: T) => Promise<void>;

  constructor(run: (job: T) => Promise<void>) {
    this.run = run;
  }

  request(job: T): void {
    if (this.running) {
      this.waiting = job;
      return;
    }
    this.running = true;
    void this.run(job)
      .catch(() => undefined)
      .finally(() => {
        this.running = false;
        const next = this.waiting;
        this.waiting = null;
        if (next !== null) this.request(next);
      });
  }
}

const pictures = new LruCache<string>(THUMB_LIMIT);

/** Decodes one thumbnail MP4 into a small JPEG data URL. */
function decode(url: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const video = document.createElement('video');
    video.muted = true;
    video.playsInline = true;
    video.preload = 'auto';
    const timer = window.setTimeout(() => finish(new Error('timeout')), 8000);
    function finish(err: Error | null, picture?: string) {
      window.clearTimeout(timer);
      video.removeAttribute('src');
      video.load();
      if (err || picture === undefined) reject(err ?? new Error('no picture'));
      else resolve(picture);
    }
    video.addEventListener('error', () => finish(new Error('The picture could not be loaded.')));
    video.addEventListener('loadeddata', () => {
      try {
        const w = THUMB_WIDTH;
        const h = Math.max(1, Math.round((w * (video.videoHeight || 9)) / (video.videoWidth || 16)));
        const canvas = document.createElement('canvas');
        canvas.width = w;
        canvas.height = h;
        const ctx = canvas.getContext('2d');
        if (!ctx) throw new Error('no canvas');
        ctx.drawImage(video, 0, 0, w, h);
        finish(null, canvas.toDataURL('image/jpeg', 0.7));
      } catch (err) {
        finish(err instanceof Error ? err : new Error('The picture could not be drawn.'));
      }
    });
    video.src = url;
  });
}

interface ThumbJob {
  segmentId: number;
  offsetMs: number;
  done: (picture: string | null) => void;
}

/** Thumbnails of one camera: one request in flight, the latest position wins. */
export class Thumbnails {
  private readonly queue = new LatestOnly<ThumbJob>(async (job) => {
    const { key, tMs } = thumbKey(job.segmentId, job.offsetMs);
    try {
      const picture = await decode(api.playThumbUrl(job.segmentId, tMs));
      pictures.set(key, picture);
      job.done(picture);
    } catch {
      job.done(null); // H.265 in a browser without it, or no copy reachable: no picture
    }
  });

  /** The picture if it is already known. */
  cached(segmentId: number, offsetMs: number): string | null {
    return pictures.get(thumbKey(segmentId, offsetMs).key) ?? null;
  }

  /** Asks for a picture; `done` is called once, unless a newer request replaces this one. */
  request(segmentId: number, offsetMs: number, done: (picture: string | null) => void): void {
    const known = this.cached(segmentId, offsetMs);
    if (known !== null) {
      done(known);
      return;
    }
    this.queue.request({ segmentId, offsetMs, done });
  }
}
