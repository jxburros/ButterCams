// The recordings player (spec 16.5, docs/V2_PLAN.md M2 step 4 and M4).
//
// The browser builds the continuous stream itself: for each hour file it asks
// for a small index, fetches keyframe-aligned chunks with ordinary HTTP range
// requests and appends them through Media Source Extensions. The server holds
// no playback state. No React in here.

import { api, ApiError } from '../api/client';
import type { PlayIndex } from '../api/types';
import { attachMediaSource, isTypeSupported, mseAvailable, NO_MSE_MESSAGE } from './mse';
import type { AttachedSource } from './mse';
import type { SyncPlayer } from './SyncGroup';
import {
  audioAllowed,
  BUFFER_BEHIND_S,
  dropWindow,
  needsMore,
  PREFETCH_INDEX_S,
  trimRange,
  wantKeyframes,
} from './timelineLogic';
import type { QualitySample } from './timelineLogic';
import {
  anchor,
  chunkAt,
  chunkRun,
  firstFrameMs,
  gapMessage,
  mediaToWall,
  nextSpan,
  placedAt,
  placeNext,
  seamKeep,
  seekTarget,
  wallToMedia,
} from './timelineMap';
import type { Placed, PlacedGap, Span } from './timelineMap';

export type PlayerStatus =
  | 'idle' // nothing chosen yet
  | 'loading' // fetching an index or the first chunk
  | 'playing'
  | 'paused'
  | 'buffering' // wants to play, waits for data
  | 'norecording' // no recording at the requested moment
  | 'ended' // played past the last recording
  | 'unsupported' // the browser cannot play this file (H.265, or no MSE)
  | 'error';

export interface PlayerState {
  status: PlayerStatus;
  /** Wall-clock position, epoch ms. */
  wallMs: number | null;
  /** What to tell the viewer: an error, a skipped gap, "cannot play H.265". */
  message: string | null;
  /** The file under the playhead (for Download). */
  segmentId: number | null;
  /** `true` while only keyframes are shown (very fast playback, scrubbing). */
  keyframes: boolean;
  rate: number;
  /** `true` when the position is at the newest video of the file being recorded. */
  atLiveEdge: boolean;
}

export interface TimelinePlayerOptions {
  onState: (state: PlayerState) => void;
  /** The camera's reported frame rate, for frame steps when the browser cannot measure it. */
  fps?: number;
  /**
   * Called when the player reached the newest file it knows of while that
   * file was still being recorded moments ago: the page should fetch the
   * timeline again, since the next hour's file has probably started.
   */
  onWantSpans?: () => void;
}

const TICK_MS = 250;
/** One range request holds at most this much. */
const RUN_BYTES = 6 << 20;
const RUN_MS = 12_000;
/** Keyframe fragments asked for in one request. */
const KEYFRAME_RUN = 60;
const OPEN_REFRESH_MS = 2000;
/** Most bytes of video kept ahead of the playhead in full-frame playback. */
const MAX_AHEAD_BYTES = 64 << 20;

/** The average size of a second of a file's video. */
function bytesPerSecond(index: PlayIndex): number {
  let bytes = 0;
  let ms = 0;
  for (const c of index.chunks) {
    ms += c[1];
    bytes += c[3];
  }
  return ms > 0 ? (bytes * 1000) / ms : 0;
}

/** A file that closed this recently may still be followed by one the page does not know of yet. */
const RECENT_CLOSE_MS = 120_000;
/** Seams and re-anchored timestamps may leave holes this small; they are stepped over. */
const NUDGE_S = 0.75;

interface Loaded {
  span: Span;
  index: PlayIndex;
  placed: Placed;
  /** Next chunk to append. */
  next: number;
  /** When the open file's index was last fetched. */
  refreshedAt: number;
  /** Chunks to play; fewer than all when the next file repeats the end (rollover). Unset until decided. */
  limit?: number;
  /** Duration of one video frame, ms; 0 until a chunk has been read. */
  frameMs: number;
  /** Keyframe mode: the next request must bring the video-only header. */
  needInit: boolean;
}

/** The tail of a file is decided when this few chunks are left to append. */
const TAIL_CHUNKS = 3;

class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

type VideoWithCallbacks = HTMLVideoElement & {
  requestVideoFrameCallback?: (cb: (now: number, meta: { mediaTime: number }) => void) => number;
  getVideoPlaybackQuality?: () => { droppedVideoFrames: number; totalVideoFrames: number };
};

function waitUpdate(sb: SourceBuffer): Promise<void> {
  if (!sb.updating) return Promise.resolve();
  return new Promise((resolve) => sb.addEventListener('updateend', () => resolve(), { once: true }));
}

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error && err.message) return err.message;
  return 'The recording could not be loaded.';
}

/** Plays the recordings of one camera on a `<video>` element. */
export class TimelinePlayer implements SyncPlayer {
  private readonly video: VideoWithCallbacks;
  private readonly opts: TimelinePlayerOptions;

  private spans: Span[] = [];
  private attached: AttachedSource | null = null;
  private sb: SourceBuffer | null = null;
  private mime = '';

  private chain: Loaded[] = [];
  private gaps: PlacedGap[] = [];
  /** A file with another codec or track set follows: restart there when the playhead arrives. */
  private restartAt: Span | null = null;
  private noMore = false;
  /** When onWantSpans was last called. */
  private spansWantedAt = 0;
  /** No appends before this time: the browser's buffer was full. */
  private fullUntil = 0;

  private generation = 0;
  private pumpQueued = false;
  /** Everything that touches the SourceBuffer runs one after the other. */
  private lock: Promise<void> = Promise.resolve();
  private abort: AbortController | null = null;
  private messageUntil = 0;
  private timer: number | null = null;
  private destroyed = false;

  private wantPlay = false;
  private rate = 1;
  private trim = 1;
  private userMuted = true;
  private keyframes = false;
  private scrubbing = false;
  private status: PlayerStatus = 'idle';
  private message: string | null = null;
  private positionMs: number | null = null; // wall position while nothing is loaded
  private quality: QualitySample[] = [];
  private frameDuration = 0;
  private lastFrameTime = -1;
  private readonly indexCache = new Map<number, PlayIndex>();
  private lastEmit = '';

  constructor(video: HTMLVideoElement, opts: TimelinePlayerOptions) {
    this.video = video;
    this.opts = opts;
    video.muted = true;
    video.playsInline = true;
    video.addEventListener('waiting', this.onVideoEvent);
    video.addEventListener('playing', this.onVideoEvent);
    video.addEventListener('pause', this.onVideoEvent);
    video.addEventListener('ended', this.onVideoEvent);
    video.addEventListener('error', this.onVideoError);
    this.timer = window.setInterval(() => this.tick(), TICK_MS);
    this.watchFrames();
  }

  // ------------------------------------------------------------ public API

  /** The recorded spans of the camera, oldest first (from GET /timeline). */
  setSpans(spans: Span[]): void {
    this.spans = spans;
    // A span that was open and is now closed lets the player move on.
    for (const item of this.chain) {
      const fresh = spans.find((s) => s.id === item.span.id);
      if (fresh) item.span = fresh;
    }
    if (this.noMore && this.chain.length > 0) this.noMore = false;
  }

  wallTime(): number | null {
    if (this.chain.length === 0 || !this.sb) return this.positionMs;
    return mediaToWall(
      this.chain.map((c) => c.placed),
      this.video.currentTime,
    );
  }

  hasRecordingAt(wallMs: number): boolean {
    return seekTarget(this.spans, wallMs, Date.now()).kind === 'inside';
  }

  isBuffering(): boolean {
    return this.status === 'buffering' || this.status === 'loading';
  }

  isLoaded(): boolean {
    return this.status === 'playing' || this.status === 'paused' || this.status === 'buffering';
  }

  hasFailed(): boolean {
    return this.status === 'error' || this.status === 'unsupported';
  }

  play(): void {
    this.wantPlay = true;
    if (this.status === 'ended' || this.status === 'norecording' || this.status === 'idle') {
      const at = this.positionMs ?? this.spans[0]?.startMs;
      if (at !== undefined) this.seekWall(at, true);
      return;
    }
    void this.video.play().catch(() => undefined);
    this.emit();
  }

  pause(): void {
    this.wantPlay = false;
    this.video.pause();
    this.emit();
  }

  /** The speed the viewer chose: 0.5 to 16. */
  setRate(rate: number): void {
    this.rate = rate;
    this.quality = [];
    this.applyRate();
    if (this.keyframes && !this.scrubbing && !wantKeyframes(rate, true, 0, 0)) this.setKeyframes(false);
    this.emit();
  }

  /** A small speed correction from the sync group; 1 = none. */
  setTrim(factor: number): void {
    if (this.trim === factor) return;
    this.trim = factor;
    this.applyRate();
  }

  setMuted(muted: boolean): void {
    this.userMuted = muted;
    this.applyRate();
  }

  /**
   * Fast scrubbing while the viewer drags: keyframes only. Turning it off
   * returns to full playback at the current position.
   */
  setScrubbing(on: boolean): void {
    if (this.scrubbing === on) return;
    this.scrubbing = on;
    if (on) this.setKeyframes(true);
    else if (!wantKeyframes(this.rate, false, 0, 0)) this.setKeyframes(false);
  }

  /** One frame forward or back while paused. */
  step(direction: 1 | -1): void {
    if (!this.sb || this.chain.length === 0) return;
    this.pause();
    const frame = this.frameDuration > 0 ? this.frameDuration : 1 / (this.opts.fps && this.opts.fps > 0 ? this.opts.fps : 15);
    const target = this.video.currentTime + direction * frame;
    const buffered = this.video.buffered;
    for (let i = 0; i < buffered.length; i++) {
      if (target >= buffered.start(i) && target < buffered.end(i)) {
        this.video.currentTime = target;
        this.emit();
        return;
      }
    }
    const wall = this.wallTime();
    if (wall !== null) this.seekWall(wall + direction * frame * 1000, false);
  }

  showNoRecording(wallMs: number): void {
    this.newGeneration();
    this.wantPlay = false;
    this.video.pause();
    this.positionMs = wallMs;
    this.teardown();
    this.setStatus('norecording', 'No recording at this moment');
  }

  /**
   * Plays (or shows, when `play` is false) the recording at a wall-clock
   * time. A time inside a gap goes to the next recording and says what was
   * skipped.
   */
  seekWall(wallMs: number, play: boolean): void {
    if (this.destroyed) return;
    this.wantPlay = play;
    this.positionMs = wallMs;

    // Already buffered: just move.
    if (this.sb && this.chain.length > 0 && !this.pendingReset) {
      const media = wallToMedia(
        this.chain.map((c) => c.placed),
        wallMs,
      );
      if (media !== null && this.isBuffered(media)) {
        this.video.currentTime = media;
        if (play) void this.video.play().catch(() => undefined);
        this.message = null;
        this.emit();
        return;
      }
    }
    const generation = this.newGeneration();
    this.pendingReset = true;
    void this.serial(() => this.load(wallMs, generation)).catch((err: unknown) => {
      if (generation === this.generation) this.setStatus('error', describe(err));
    });
  }

  destroy(): void {
    this.destroyed = true;
    this.newGeneration();
    if (this.timer !== null) window.clearInterval(this.timer);
    this.timer = null;
    this.video.removeEventListener('waiting', this.onVideoEvent);
    this.video.removeEventListener('playing', this.onVideoEvent);
    this.video.removeEventListener('pause', this.onVideoEvent);
    this.video.removeEventListener('ended', this.onVideoEvent);
    this.video.removeEventListener('error', this.onVideoError);
    this.teardown();
  }

  // -------------------------------------------------------------- loading

  private pendingReset = false;

  /** Starts a new generation: work of the old one stops at its next step. */
  private newGeneration(): number {
    this.abort?.abort();
    this.abort = typeof AbortController === 'undefined' ? null : new AbortController();
    return ++this.generation;
  }

  private serial<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.lock.then(fn);
    this.lock = run.then(
      () => undefined,
      () => undefined,
    );
    return run;
  }

  private async load(wallMs: number, generation: number): Promise<void> {
    const stale = () => generation !== this.generation || this.destroyed;
    if (!mseAvailable()) {
      this.setStatus('unsupported', NO_MSE_MESSAGE);
      return;
    }
    const target = seekTarget(this.spans, wallMs, Date.now());
    if (target.kind === 'none') {
      this.video.pause();
      this.teardown();
      this.setStatus(this.spans.length === 0 ? 'norecording' : 'ended', this.spans.length === 0 ? 'No recordings on this day' : 'No recording after this moment');
      return;
    }
    const skipped = target.kind === 'skipped' ? gapMessage(target.skippedFromMs, target.skippedToMs) : null;
    this.pendingReset = true;
    this.setStatus('loading', skipped);

    const span = target.span;
    const index = await this.fetchIndex(span);
    if (stale()) return;
    // Keyframe-only fragments carry no audio: they need a video-only source.
    const mime = this.mimeFor(index);
    if (!isTypeSupported(mime)) {
      this.video.pause();
      this.teardown();
      this.positionMs = Math.max(wallMs, span.startMs);
      this.unsupportedId = span.id;
      this.setStatus(
        'unsupported',
        span.videoCodec === 'h265'
          ? 'This browser cannot play H.265 video. Download the file to watch it, or use Safari or a recent Chrome on a device with H.265 support.'
          : 'This browser cannot play this recording. Download the file to watch it.',
      );
      return;
    }
    this.unsupportedId = null;

    // One MediaSource per MIME string; a seek inside the same kind of file
    // only empties the buffer.
    if (!this.sb || !this.attached || this.mime !== mime || this.attached.mediaSource.readyState !== 'open') {
      this.teardown();
      const attached = attachMediaSource(this.video);
      if (!attached) {
        this.setStatus('unsupported', NO_MSE_MESSAGE);
        return;
      }
      this.attached = attached;
      await attached.opened;
      if (stale() || this.attached !== attached || attached.mediaSource.readyState !== 'open') return;
      this.sb = attached.mediaSource.addSourceBuffer(mime);
      this.sb.mode = 'segments';
      this.mime = mime;
    } else {
      const sb = this.sb;
      await waitUpdate(sb);
      try {
        sb.abort();
        sb.appendWindowStart = 0;
        sb.appendWindowEnd = Infinity;
        sb.remove(0, Infinity);
      } catch {
        // an empty buffer has nothing to remove
      }
      await waitUpdate(sb);
      if (stale()) return;
    }

    const placed = anchor(span, index);
    const offsetMs = Math.max(0, Math.max(wallMs, placed.wallStartMs) - placed.wallStartMs);
    let first = Math.max(0, chunkAt(index.chunks, offsetMs));
    // The newest chunks of the file being recorded may not be listed yet: stay inside.
    const startS = Math.min(offsetMs, Math.max(0, index.durationMs - 200)) / 1000;
    if (index.chunks.length === 0) first = 0;
    this.chain = [{ span, index, placed, next: first, refreshedAt: Date.now(), frameMs: 0, needInit: true }];
    this.gaps = [];
    this.restartAt = null;
    this.noMore = false;
    this.pendingReset = false;
    this.quality = [];

    await this.appendInit(this.chain[0] as Loaded);
    if (stale()) return;
    await this.pumpOnce(generation);
    if (stale()) return;
    this.video.currentTime = placed.mediaStart + startS;
    this.applyRate();
    if (this.wantPlay) void this.video.play().catch(() => undefined);
    this.messageUntil = skipped ? Date.now() + 8000 : 0;
    this.setStatus(this.wantPlay ? 'buffering' : 'paused', skipped);
  }

  private unsupportedId: number | null = null;

  private async fetchIndex(span: Span): Promise<PlayIndex> {
    const cached = this.indexCache.get(span.id);
    if (cached && !cached.open) return cached; // a closed file never changes
    const index = await api.playIndex(span.id);
    this.indexCache.set(span.id, index);
    if (this.indexCache.size > 16) {
      const oldest = this.indexCache.keys().next();
      if (!oldest.done) this.indexCache.delete(oldest.value);
    }
    return index;
  }

  private mimeFor(index: PlayIndex): string {
    return this.keyframes && index.videoMime ? index.videoMime : index.mime;
  }

  private async fetchBytes(url: string, range: [number, number] | null): Promise<ArrayBuffer> {
    const headers: Record<string, string> = {};
    if (range) headers.Range = `bytes=${range[0]}-${range[0] + range[1] - 1}`;
    let res: Response;
    try {
      res = await fetch(url, { headers, credentials: 'same-origin', signal: this.abort?.signal });
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') throw err;
      throw new Error('Could not reach the ButterCams server.');
    }
    if (!res.ok) {
      let message = `The recording could not be loaded (HTTP ${res.status}).`;
      try {
        const parsed = (await res.json()) as { error?: { message?: string } };
        if (parsed.error?.message) message = parsed.error.message;
      } catch {
        // not JSON
      }
      throw new HttpError(res.status, message);
    }
    return res.arrayBuffer();
  }

  private async append(data: ArrayBuffer, generation: number): Promise<boolean> {
    const sb = this.sb;
    if (!sb || generation !== this.generation) return false;
    for (let attempt = 0; attempt < 2; attempt++) {
      await waitUpdate(sb);
      if (generation !== this.generation || this.sb !== sb) return false;
      try {
        sb.appendBuffer(data);
        await waitUpdate(sb);
        return true;
      } catch (err) {
        if (err instanceof DOMException && err.name === 'QuotaExceededError' && attempt === 0) {
          // The buffer is full: drop what is behind the playhead and try once more.
          const t = this.video.currentTime;
          try {
            if (sb.buffered.length > 0 && sb.buffered.start(0) < t - 5) {
              sb.remove(sb.buffered.start(0), t - 5);
              await waitUpdate(sb);
            }
          } catch {
            // fall through to the retry
          }
          continue;
        }
        if (err instanceof DOMException && err.name === 'QuotaExceededError') {
          // Still full with nothing behind the playhead to drop: wait for the
          // playhead to use some up, then try again.
          this.fullUntil = Date.now() + 1000;
          return false;
        }
        throw err;
      }
    }
    return false;
  }

  /** Sets where the next file's samples land and appends its header. */
  private async appendInit(item: Loaded): Promise<void> {
    const sb = this.sb;
    if (!sb) return;
    const generation = this.generation;
    // In keyframe mode the header comes with the first keyframes (init=1): it
    // is a video-only one, not the file's.
    let init: ArrayBuffer | null = null;
    if (!this.keyframes) {
      const [offset, length] = item.index.init;
      init = await this.fetchBytes(api.playDataUrl(item.span.id), [offset, length]);
      if (generation !== this.generation || this.sb !== sb) return;
    }
    item.needInit = true;
    await waitUpdate(sb);
    try {
      sb.abort(); // resets the parser between files
    } catch {
      // not open any more
    }
    sb.timestampOffset = item.placed.mediaStart;
    this.setAppendWindow(item);
    if (init) await this.append(init, generation);
  }

  /**
   * Before the last chunks of a closed file are appended: if the next file
   * repeats them (the hourly rollover), leave them out.
   */
  private async decideTail(head: Loaded, generation: number): Promise<void> {
    const total = head.index.chunks.length;
    const following = nextSpan(this.spans, head.span.id);
    if (!following) return; // not known yet; decided when it appears, if chunks are left
    head.limit = total;
    if (following.gapBefore) return;
    const next = await this.fetchIndex(following);
    if (generation !== this.generation) return;
    const parsed = next.start ? Date.parse(next.start) : NaN;
    const nextStart = Number.isNaN(parsed) ? following.startMs : parsed;
    const frameMs = head.frameMs > 0 ? head.frameMs : this.frameDuration > 0 ? this.frameDuration * 1000 : 1000 / (this.opts.fps && this.opts.fps > 0 ? this.opts.fps : 15);
    const { keep, keptMs } = seamKeep(head.index.chunks, head.placed.wallStartMs, head.index.durationMs, nextStart, frameMs);
    if (keep >= total) return;
    const cutAt = head.placed.mediaStart + keptMs / 1000;
    if (keep < head.next) {
      // The repeated chunks are already in the buffer (playing at the newest
      // video when the hour turned): take them out again, unless the playhead
      // is already in them.
      const sb = this.sb;
      if (!sb || this.video.currentTime >= cutAt - 0.1) return;
      await waitUpdate(sb);
      try {
        sb.remove(cutAt, head.placed.mediaStart + head.placed.duration + 1);
        await waitUpdate(sb);
      } catch {
        return;
      }
      head.next = keep;
    }
    head.limit = keep;
    head.placed.duration = keptMs / 1000;
    this.setAppendWindow(head);
  }

  /** Nothing of a file may land outside its place: the rollover overlap is cut here. */
  private setAppendWindow(item: Loaded): void {
    const sb = this.sb;
    if (!sb) return;
    try {
      sb.appendWindowStart = 0;
      sb.appendWindowEnd = item.index.open ? Infinity : item.placed.mediaStart + item.placed.duration + 0.0005;
    } catch {
      // while updating; the next file sets it again
    }
  }

  // -------------------------------------------------------------- pumping

  private pump(): void {
    if (this.pumpQueued || this.destroyed) return;
    this.pumpQueued = true;
    const generation = this.generation;
    void this.serial(async () => {
      try {
        // A few rounds per call; the tick calls again.
        for (let i = 0; i < 4 && generation === this.generation; i++) {
          if (!(await this.pumpOnce(generation))) break;
        }
      } catch (err) {
        if (generation === this.generation && !(err instanceof DOMException && err.name === 'AbortError')) {
          this.wantPlay = false;
          this.video.pause();
          this.setStatus('error', describe(err));
        }
      } finally {
        this.pumpQueued = false;
      }
    });
  }

  /** Appends the next piece if the buffer needs it. Returns whether it did something. */
  private async pumpOnce(generation: number): Promise<boolean> {
    const sb = this.sb;
    const head = this.chain[this.chain.length - 1];
    if (!sb || !head || this.pendingReset || generation !== this.generation) return false;
    const ms = this.attached?.mediaSource;
    if (!ms || ms.readyState === 'closed') return false;
    if (ms.readyState === 'ended' && this.noMore) return false;

    const t = this.video.currentTime;
    const end = this.bufferedEnd();
    const headEnd = head.placed.mediaStart + head.placed.duration;

    // Old video behind the playhead goes.
    if (sb.buffered.length > 0) {
      const range = trimRange(sb.buffered.start(0), t);
      if (range && !sb.updating) {
        try {
          sb.remove(range[0], range[1]);
          await waitUpdate(sb);
        } catch {
          // try again next round
        }
      }
    }

    if (!head.index.open && head.limit === undefined && head.next >= head.index.chunks.length - TAIL_CHUNKS) {
      await this.decideTail(head, generation);
      if (generation !== this.generation) return false;
    }
    const limit = head.index.open ? head.index.chunks.length : (head.limit ?? head.index.chunks.length);

    if (head.next < limit) {
      if (!needsMore(t, Math.max(end, t), this.rate)) return false;
      if (Date.now() < this.fullUntil) return false;
      // Fast playback of a high-bitrate stream would otherwise ask the
      // browser to hold more than its buffer allows (about 150 MB in Chrome).
      if (!this.keyframes && (Math.max(end, t) - t) * bytesPerSecond(head.index) > MAX_AHEAD_BYTES) return false;
      try {
        if (this.keyframes) {
          const last = Math.min(head.next + KEYFRAME_RUN, limit) - 1;
          const a = head.index.chunks[head.next];
          const b = head.index.chunks[last];
          if (!a || !b) return false;
          // The server returns every chunk that overlaps [from, to): stay
          // strictly inside the wanted ones.
          const from = (a[0] + Math.min(1, a[1] / 2)).toFixed(3);
          const to = (b[0] + b[1] - Math.min(1, b[1] / 2)).toFixed(3);
          const url = `${api.playDataUrl(head.span.id).replace(/data$/, 'keyframes')}?from=${from}&to=${to}${head.needInit ? '&init=1' : ''}`;
          const data = await this.fetchBytes(url, null);
          if (!(await this.append(data, generation))) return false;
          head.needInit = false;
          head.next = last + 1;
        } else {
          const run = chunkRun(head.index.chunks.slice(0, limit), head.next, RUN_BYTES, RUN_MS);
          if (!run) return false;
          const data = await this.fetchBytes(api.playDataUrl(head.span.id), [run.offset, run.size]);
          if (head.frameMs === 0) head.frameMs = firstFrameMs(data);
          if (!(await this.append(data, generation))) return false;
          head.next += run.count;
        }
      } catch (err) {
        // The file being recorded: the index can be a moment ahead of what
        // can be read, or the file was closed and renamed meanwhile. Ask again.
        if (err instanceof HttpError && (err.status === 416 || err.status === 404) && head.index.open) {
          head.refreshedAt = 0;
          const index = await api.playIndex(head.span.id);
          if (generation !== this.generation) return false;
          this.indexCache.set(head.span.id, index);
          head.index = index;
          head.placed.duration = index.durationMs / 1000;
          head.refreshedAt = Date.now();
          return false;
        }
        throw err;
      }
      return true;
    }

    // The head file is used up.
    if (head.index.open) {
      // The hour in progress: ask for more while the playhead is near its end.
      const now = Date.now();
      if (headEnd - t > 30 || now - head.refreshedAt < OPEN_REFRESH_MS) return false;
      head.refreshedAt = now;
      const index = await api.playIndex(head.span.id);
      if (generation !== this.generation) return false;
      this.indexCache.set(head.span.id, index);
      const grew = index.chunks.length > head.index.chunks.length || index.open !== head.index.open;
      head.index = index;
      head.placed.duration = index.durationMs / 1000;
      if (!index.open) this.setAppendWindow(head);
      return grew;
    }

    if (this.restartAt || this.noMore) return false;
    if (headEnd - t > PREFETCH_INDEX_S && !needsMore(t, Math.max(end, t), this.rate)) return false;

    const following = nextSpan(this.spans, head.span.id);
    if (!following) {
      // The hour being recorded just closed: its successor exists but the
      // page's list of files is older than that. Ask for a new list and keep
      // waiting instead of ending playback at the top of the hour.
      const now = Date.now();
      const recent = head.span.open || head.span.endMs === null || now - head.span.endMs < RECENT_CLOSE_MS;
      if (recent) {
        if (now - this.spansWantedAt >= OPEN_REFRESH_MS) {
          this.spansWantedAt = now;
          this.opts.onWantSpans?.();
        }
        return false;
      }
      // Nothing follows: tell the browser, so it plays the last frames out
      // instead of waiting for more.
      this.noMore = true;
      await waitUpdate(sb);
      try {
        if (ms.readyState === 'open') ms.endOfStream();
      } catch {
        // already ended or closed
      }
      return false;
    }
    const index = await this.fetchIndex(following);
    if (generation !== this.generation) return false;
    if (this.mimeFor(index) !== this.mime) {
      // Another codec, resolution or set of tracks: a new media source starts
      // at the boundary (a short blink is acceptable there).
      this.restartAt = following;
      return false;
    }
    const { placed, gap } = placeNext(head.placed, following, index);
    const item: Loaded = { span: following, index, placed, next: 0, refreshedAt: Date.now(), frameMs: 0, needInit: true };
    this.chain.push(item);
    if (gap) this.gaps.push(gap);
    // Forget files that are entirely behind the buffer.
    while (this.chain.length > 3) {
      const old = this.chain[0];
      if (!old || old.placed.mediaStart + old.placed.duration > t - BUFFER_BEHIND_S) break;
      this.chain.shift();
    }
    await this.appendInit(item);
    return true;
  }

  // ---------------------------------------------------------------- state

  private bufferedEnd(): number {
    const b = this.video.buffered;
    const t = this.video.currentTime;
    let end = t;
    for (let i = 0; i < b.length; i++) {
      // Ranges that touch (a seam, a skipped gap) count as one run ahead.
      if (b.start(i) <= end + NUDGE_S + 0.01 && b.end(i) > end) end = b.end(i);
      else if (this.gaps.some((g) => Math.abs(g.mediaFrom - end) < NUDGE_S && Math.abs(b.start(i) - g.mediaTo) < NUDGE_S)) end = b.end(i);
    }
    return end;
  }

  private isBuffered(media: number): boolean {
    const b = this.video.buffered;
    for (let i = 0; i < b.length; i++) {
      if (media >= b.start(i) && media < b.end(i) - 0.05) return true;
    }
    return false;
  }

  private applyRate(): void {
    const rate = this.rate * this.trim;
    if (this.video.playbackRate !== rate) {
      try {
        this.video.playbackRate = rate;
      } catch {
        // a speed this browser refuses: keep the old one
      }
    }
    this.video.muted = this.userMuted || !audioAllowed(this.rate) || this.keyframes;
  }

  private setKeyframes(on: boolean): void {
    if (this.keyframes === on) return;
    this.keyframes = on;
    this.quality = [];
    const wall = this.wallTime();
    if (wall !== null && this.sb && this.chain.length > 0) {
      // The buffer holds the other kind of fragments: start again from here.
      this.pendingReset = true;
      this.seekWall(wall, this.wantPlay);
    }
    this.applyRate();
    this.emit();
  }

  private tick(): void {
    if (this.destroyed) return;
    const video = this.video;
    if (this.sb && this.chain.length > 0 && !this.pendingReset) {
      const t = video.currentTime;
      // Browsers stop a little before the end of what is buffered when
      // nothing follows directly (the tracks of a file rarely end together).
      const stalled = this.wantPlay && video.readyState < 3;
      // A real gap: skip it and say what was skipped.
      for (const gap of this.gaps) {
        const reached = t >= gap.mediaFrom - 0.08 || (stalled && t >= gap.mediaFrom - 1.5);
        if (reached && t < gap.mediaTo && this.isBuffered(gap.mediaTo + 0.01)) {
          video.currentTime = gap.mediaTo + 0.01;
          this.message = gapMessage(gap.wallFromMs, gap.wallToMs);
          this.messageUntil = Date.now() + 8000;
          break;
        }
      }
      // A small hole (a seam that did not line up to the frame): step over it.
      if (stalled) {
        const b = video.buffered;
        for (let i = 0; i < b.length; i++) {
          if (b.start(i) > t && b.start(i) - t < NUDGE_S) {
            video.currentTime = b.start(i) + 0.01;
            break;
          }
        }
      }
      // The next file needs a new media source: restart there when the
      // playhead has reached the end of this one.
      const head = this.chain[this.chain.length - 1];
      if (head && this.restartAt) {
        const headEnd = head.placed.mediaStart + head.placed.duration;
        if (t >= headEnd - 0.15 || (stalled && this.bufferedEnd() - t < 1)) {
          const to = this.restartAt;
          this.restartAt = null;
          this.pendingReset = true;
          this.seekWall(to.startMs, this.wantPlay);
          return;
        }
      }
      // Past the last recording.
      if (head && this.noMore && !head.index.open && this.wantPlay) {
        const headEnd = head.placed.mediaStart + head.placed.duration;
        if (video.ended || t >= headEnd - 0.15 || (stalled && this.bufferedEnd() - t < 1)) {
          this.wantPlay = false;
          video.pause();
          this.setStatus('ended', 'End of the recordings');
          return;
        }
      }
      // Above 4×: every frame while the browser keeps up, else keyframes only.
      if (this.wantPlay && !this.scrubbing && this.rate > 1 && video.getVideoPlaybackQuality) {
        const q = video.getVideoPlaybackQuality();
        const w = dropWindow(this.quality, { atMs: performance.now(), dropped: q.droppedVideoFrames, total: q.totalVideoFrames });
        if (w.full && wantKeyframes(this.rate, this.keyframes, w.dropped, w.total) !== this.keyframes) {
          this.setKeyframes(!this.keyframes);
          return;
        }
      }
      if (this.messageUntil !== 0 && Date.now() > this.messageUntil && this.status === 'playing') {
        this.message = null;
        this.messageUntil = 0;
      }
      if (this.status !== 'error' && this.status !== 'loading' && !(this.status === 'ended' && !this.wantPlay)) {
        if (!this.wantPlay) this.status = 'paused';
        else this.status = video.readyState >= 3 && !video.paused ? 'playing' : 'buffering';
        if (this.wantPlay && video.paused && video.readyState >= 3) void video.play().catch(() => undefined);
      }
      this.pump();
    }
    this.emit();
  }

  private readonly onVideoEvent = (): void => {
    if (!this.destroyed) this.emit();
  };

  private readonly onVideoError = (): void => {
    if (this.destroyed || !this.video.error || this.chain.length === 0) return;
    this.wantPlay = false;
    this.unsupportedId = this.chain[this.chain.length - 1]?.span.id ?? null;
    this.setStatus('error', 'The browser could not play this part of the recording. Download the file to watch it.');
  };

  /** Measures the time between frames while playing, for frame steps. */
  private watchFrames(): void {
    const video = this.video;
    if (!video.requestVideoFrameCallback) return;
    const onFrame = (_now: number, meta: { mediaTime: number }) => {
      if (this.destroyed) return;
      if (this.lastFrameTime >= 0 && !this.keyframes) {
        const d = meta.mediaTime - this.lastFrameTime;
        if (d > 0.001 && d < 0.5) this.frameDuration = this.frameDuration === 0 ? d : Math.min(this.frameDuration, d);
      }
      this.lastFrameTime = meta.mediaTime;
      video.requestVideoFrameCallback?.(onFrame);
    };
    video.requestVideoFrameCallback(onFrame);
  }

  private teardown(): void {
    const attached = this.attached;
    this.attached = null;
    this.sb = null;
    this.mime = '';
    this.chain = [];
    this.gaps = [];
    this.restartAt = null;
    this.pendingReset = false;
    attached?.detach();
  }

  private setStatus(status: PlayerStatus, message: string | null): void {
    this.status = status;
    this.message = message;
    this.emit();
  }

  private emit(): void {
    const wallMs = this.wallTime();
    if (wallMs !== null && this.chain.length > 0) this.positionMs = wallMs;
    const head = this.chain[this.chain.length - 1];
    const current = placedAt(
      this.chain.map((c) => c.placed),
      this.video.currentTime,
    );
    const state: PlayerState = {
      status: this.status,
      wallMs,
      message: this.message,
      segmentId: this.chain.length > 0 ? (current?.id ?? null) : this.unsupportedId,
      keyframes: this.keyframes,
      rate: this.rate,
      atLiveEdge:
        !!head && head.index.open && head.next >= head.index.chunks.length && this.bufferedEnd() - this.video.currentTime < 3,
    };
    // The position changes all the time; everything else rarely.
    const key = `${state.status}|${state.message}|${state.segmentId}|${state.keyframes}|${state.rate}|${state.atLiveEdge}|${
      wallMs === null ? '' : Math.round(wallMs / 100)
    }`;
    if (key === this.lastEmit) return;
    this.lastEmit = key;
    this.opts.onState(state);
  }
}
