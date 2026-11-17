// Plays one camera's live stream into a <video> element: fragmented MP4
// arrives over a WebSocket and is appended through Media Source Extensions
// (spec 16.4). The player keeps itself at the live edge, trims its buffer and
// reconnects by itself. No React in here.

import { api } from '../api/client';
import type { StreamKind } from '../api/types';
import { backoffDelay, liveEdgeAction, parseLiveMessage, probeUrl, trimRange } from './liveLogic';
import type { AttachedSource } from './mse';
import { NO_MSE_MESSAGE, attachMediaSource, isTypeSupported, mseAvailable } from './mse';

export type LiveStatus =
  | { kind: 'idle' }
  | { kind: 'connecting' }
  | { kind: 'playing' }
  /** The server has no video for this camera right now (offline, connecting …). */
  | { kind: 'waiting'; message: string }
  /** The browser cannot play this stream's codec; the player has stopped. */
  | { kind: 'unsupported'; mime: string }
  /** The stream could not be opened; the player retries by itself unless `final`. */
  | { kind: 'unavailable'; code: string; message: string; final: boolean }
  | { kind: 'error'; message: string };

export interface LivePlayerOptions {
  camera: string;
  stream: StreamKind;
  audio: boolean;
  onStatus: (status: LiveStatus) => void;
  /** Called with the picture size of every `init` message. */
  onSize?: (width: number, height: number) => void;
}

/** Unsent data beyond this means the browser is not keeping up: start again at a keyframe. */
const MAX_QUEUE_BYTES = 8 << 20;
const MAX_QUEUE_ITEMS = 900;
const TICK_MS = 500;
/** Error codes after which retrying quickly makes no sense. */
const FINAL_CODES = new Set(['live_disabled', 'forbidden', 'not_found', 'unauthorized']);

function isQuotaError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'QuotaExceededError';
}

export class LivePlayer {
  private readonly video: HTMLVideoElement;
  private readonly options: LivePlayerOptions;
  private readonly url: string;

  private running = false;
  private socket: WebSocket | null = null;
  private attached: AttachedSource | null = null;
  private buffer: SourceBuffer | null = null;
  /** Bumped whenever the media source is replaced, so late callbacks are ignored. */
  private generation = 0;
  private queue: ArrayBuffer[] = [];
  private queueBytes = 0;
  private streaming = true; // ManagedMediaSource: false between endstreaming and startstreaming
  private attempt = 0;
  private gotData = false;
  private retryTimer: number | null = null;
  private tickTimer: number | null = null;
  private lastTime = -1;
  private status: LiveStatus = { kind: 'idle' };

  constructor(video: HTMLVideoElement, options: LivePlayerOptions) {
    this.video = video;
    this.options = options;
    this.url = api.liveUrl(options.camera, options.stream, options.audio);
  }

  /** Opens the stream. Safe to call again after `stop`. */
  start(): void {
    if (this.running) return;
    this.running = true;
    if (!mseAvailable()) {
      this.setStatus({ kind: 'error', message: NO_MSE_MESSAGE });
      this.running = false;
      return;
    }
    this.attempt = 0;
    this.connect();
    this.tickTimer = window.setInterval(() => this.tick(), TICK_MS);
  }

  /** Closes the stream and releases the buffer. */
  stop(): void {
    this.running = false;
    if (this.retryTimer !== null) window.clearTimeout(this.retryTimer);
    if (this.tickTimer !== null) window.clearInterval(this.tickTimer);
    this.retryTimer = null;
    this.tickTimer = null;
    this.closeSocket();
    this.releaseSource();
    this.status = { kind: 'idle' };
  }

  private setStatus(status: LiveStatus): void {
    const prev = this.status;
    if (
      prev.kind === status.kind &&
      (status.kind !== 'waiting' || (prev as typeof status).message === status.message) &&
      (status.kind !== 'unavailable' || (prev as typeof status).message === status.message)
    ) {
      return;
    }
    this.status = status;
    this.options.onStatus(status);
  }

  // ------------------------------------------------------------ connection

  private connect(): void {
    if (!this.running) return;
    this.closeSocket();
    this.gotData = false;
    if (this.status.kind !== 'unavailable' && this.status.kind !== 'waiting') this.setStatus({ kind: 'connecting' });
    let socket: WebSocket;
    try {
      socket = new WebSocket(this.url);
    } catch {
      this.retry(false);
      return;
    }
    socket.binaryType = 'arraybuffer';
    this.socket = socket;
    let opened = false;
    socket.onopen = () => {
      opened = true;
    };
    socket.onmessage = (event: MessageEvent) => {
      if (socket !== this.socket) return;
      if (typeof event.data === 'string') this.onText(event.data);
      else if (event.data instanceof ArrayBuffer) this.onBinary(event.data);
    };
    socket.onclose = () => {
      if (socket !== this.socket) return;
      this.socket = null;
      this.retry(!opened);
    };
    socket.onerror = () => {
      // A close event always follows; it does the retry.
    };
  }

  private closeSocket(): void {
    const socket = this.socket;
    this.socket = null;
    if (socket) {
      socket.onmessage = null;
      socket.onclose = null;
      socket.onerror = null;
      try {
        socket.close();
      } catch {
        // already closed
      }
    }
  }

  /** Schedules the next connection attempt (1, 2, 5, 10 s). */
  private retry(refused: boolean): void {
    if (!this.running || this.retryTimer !== null) return;
    if (this.gotData) this.attempt = 0;
    const delay = backoffDelay(this.attempt);
    this.attempt += 1;
    if (refused) void this.explainRefusal();
    else if (this.status.kind === 'playing' || this.status.kind === 'connecting') this.setStatus({ kind: 'connecting' });
    this.retryTimer = window.setTimeout(() => {
      this.retryTimer = null;
      this.connect();
    }, delay);
  }

  /**
   * A WebSocket that is refused gives no reason. The same address asked over
   * plain HTTP answers with the API's error, which the tile can show.
   */
  private async explainRefusal(): Promise<void> {
    let code = 'network';
    let message = 'Live view is not reachable right now. Trying again…';
    try {
      const res = await fetch(probeUrl(this.url), { credentials: 'same-origin', cache: 'no-store' });
      if (!res.ok) {
        const body: unknown = await res.json().catch(() => null);
        const error = (body as { error?: { code?: unknown; message?: unknown } } | null)?.error;
        if (typeof error?.code === 'string') code = error.code;
        if (typeof error?.message === 'string' && error.message !== '') message = error.message;
        else if (res.status === 401) (code = 'unauthorized'), (message = 'Your session has ended. Sign in again.');
      } else {
        void res.body?.cancel();
      }
    } catch {
      // keep the network message
    }
    if (!this.running || this.status.kind === 'playing') return;
    const final = FINAL_CODES.has(code);
    this.setStatus({ kind: 'unavailable', code, message, final });
    if (final) {
      // Keep trying, but slowly: an Admin may switch live view back on.
      this.attempt = Math.max(this.attempt, 3);
    }
  }

  // --------------------------------------------------------------- messages

  private onText(text: string): void {
    const msg = parseLiveMessage(text);
    if (msg === null) return;
    if (msg.type === 'state') {
      this.setStatus({ kind: 'waiting', message: msg.message });
      return;
    }
    this.options.onSize?.(msg.width, msg.height);
    if (!isTypeSupported(msg.mime)) {
      this.setStatus({ kind: 'unsupported', mime: msg.mime });
      this.stop();
      return;
    }
    this.beginSource(msg.mime);
  }

  private onBinary(data: ArrayBuffer): void {
    if (this.attached === null) return; // nothing can be appended before an init message
    this.gotData = true;
    this.queue.push(data);
    this.queueBytes += data.byteLength;
    if (this.queueBytes > MAX_QUEUE_BYTES || this.queue.length > MAX_QUEUE_ITEMS) {
      this.restart();
      return;
    }
    this.pump();
  }

  // ----------------------------------------------------------- media source

  /** Every `init` starts a fresh media source: the next binary message is its header. */
  private beginSource(mime: string): void {
    this.releaseSource();
    const attached = attachMediaSource(this.video);
    if (attached === null) {
      this.setStatus({ kind: 'error', message: NO_MSE_MESSAGE });
      this.stop();
      return;
    }
    const generation = ++this.generation;
    this.attached = attached;
    this.streaming = true;
    if (attached.managed) {
      attached.mediaSource.addEventListener('startstreaming', () => {
        if (generation !== this.generation) return;
        this.streaming = true;
        this.pump();
      });
      attached.mediaSource.addEventListener('endstreaming', () => {
        if (generation === this.generation) this.streaming = false;
      });
    }
    this.video.muted = this.video.muted || !this.options.audio;
    void attached.opened.then(() => {
      if (generation !== this.generation) return;
      try {
        const buffer = attached.mediaSource.addSourceBuffer(mime);
        buffer.mode = 'segments';
        buffer.addEventListener('updateend', () => {
          if (generation === this.generation) this.pump();
        });
        buffer.addEventListener('error', () => {
          if (generation === this.generation) this.restart();
        });
        this.buffer = buffer;
        this.pump();
      } catch {
        this.setStatus({ kind: 'unsupported', mime });
        this.stop();
      }
    });
    void this.video.play().catch(() => undefined);
  }

  private releaseSource(): void {
    this.generation += 1;
    this.buffer = null;
    this.queue = [];
    this.queueBytes = 0;
    this.lastTime = -1;
    if (this.attached) {
      this.attached.detach();
      this.attached = null;
    }
  }

  /** Appends the next queued piece when the buffer is free. */
  private pump(): void {
    const buffer = this.buffer;
    if (buffer === null || buffer.updating || !this.streaming) return;
    const next = this.queue[0];
    if (next === undefined) return;
    try {
      buffer.appendBuffer(next);
      this.queue.shift();
      this.queueBytes -= next.byteLength;
    } catch (err) {
      if (isQuotaError(err) && this.makeRoom()) return; // updateend pumps again
      this.restart();
    }
  }

  /** The buffer is full: remove everything but the last two seconds. */
  private makeRoom(): boolean {
    const buffer = this.buffer;
    if (buffer === null || buffer.updating) return false;
    try {
      const ranges = buffer.buffered;
      if (ranges.length === 0) return false;
      const start = ranges.start(0);
      const end = Math.min(this.video.currentTime - 2, ranges.end(ranges.length - 1) - 2);
      if (end <= start) return false;
      buffer.remove(start, end);
      return true;
    } catch {
      return false;
    }
  }

  /** Drops everything and reconnects; the server starts a viewer at a keyframe. */
  private restart(): void {
    if (!this.running) return;
    this.closeSocket();
    this.releaseSource();
    this.setStatus({ kind: 'connecting' });
    this.retry(false);
  }

  // -------------------------------------------------------------- live edge

  private tick(): void {
    const video = this.video;
    const buffer = this.buffer;
    if (buffer === null) return;
    let ranges: TimeRanges;
    try {
      ranges = buffer.buffered;
    } catch {
      return; // the source was removed
    }
    if (ranges.length === 0) return;
    const start = ranges.start(0);
    const end = ranges.end(ranges.length - 1);

    // Playback that fell out of the buffer (a trimmed range, a gap after a
    // stall) continues at the live edge.
    if (video.currentTime < start || video.currentTime > end + 0.5) {
      video.currentTime = Math.max(start, end - 0.2);
    } else {
      const action = liveEdgeAction(video.currentTime, end, video.playbackRate);
      if (action.kind === 'seek') {
        video.currentTime = action.to;
        video.playbackRate = 1;
      } else if (video.playbackRate !== action.rate) {
        video.playbackRate = action.rate;
      }
    }
    if (video.paused) void video.play().catch(() => undefined);

    if (video.currentTime > this.lastTime && this.lastTime >= 0 && video.readyState >= 2) {
      this.setStatus({ kind: 'playing' });
    }
    this.lastTime = video.currentTime;

    if (!buffer.updating) {
      const trim = trimRange(start, video.currentTime);
      if (trim) {
        try {
          buffer.remove(trim[0], trim[1]);
        } catch {
          // tried again on the next tick
        }
      }
    }
  }
}
