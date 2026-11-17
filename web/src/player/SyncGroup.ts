// Synchronized playback of up to four cameras on one wall clock (spec 16.5).
// The first player drives the clock; the others are corrected against it.

/** How often followers are corrected. */
export const SYNC_INTERVAL_MS = 250;
/** Followers are kept within this of the clock. */
export const SYNC_TOLERANCE_MS = 150;
/** Beyond this a follower seeks instead of changing speed. */
export const SYNC_SEEK_MS = 1000;
/** Speed change used to close a small difference. */
export const SYNC_RATE_STEP = 0.05;
/** The clock waits at most this long for a player that is buffering. */
export const SYNC_MAX_HOLD_MS = 2000;
export const MAX_SYNC_PLAYERS = 4;

export type SyncAction =
  | { kind: 'none'; rate: number }
  /** Play slightly faster or slower until back within tolerance. */
  | { kind: 'rate'; rate: number }
  | { kind: 'seek'; wallMs: number };

/**
 * What a follower must do. `clockMs` is the shared wall clock, `playerMs` the
 * follower's own position, `baseRate` the speed everyone plays at.
 * `correcting` says whether the follower is already running at a corrected
 * speed: it then keeps correcting until it is within a quarter of the
 * tolerance, so the speed does not flap at the edge.
 */
export function syncAction(clockMs: number, playerMs: number, baseRate: number, correcting: boolean): SyncAction {
  const behind = clockMs - playerMs; // > 0: the follower is behind the clock
  const off = Math.abs(behind);
  if (off > SYNC_SEEK_MS) return { kind: 'seek', wallMs: clockMs };
  const limit = correcting ? SYNC_TOLERANCE_MS / 4 : SYNC_TOLERANCE_MS;
  if (off > limit) return { kind: 'rate', rate: baseRate * (behind > 0 ? 1 + SYNC_RATE_STEP : 1 - SYNC_RATE_STEP) };
  return { kind: 'none', rate: baseRate };
}

/**
 * Whether the clock should wait for buffering players. `bufferingSinceMs` is
 * when the longest-waiting buffering player started (or `null` when none is
 * buffering). After the hold is used up the clock runs on until that player
 * has caught up; `heldOut` carries that fact between calls.
 */
export function holdClock(bufferingSinceMs: number | null, nowMs: number): boolean {
  return bufferingSinceMs !== null && nowMs - bufferingSinceMs < SYNC_MAX_HOLD_MS;
}

/** How often a player that failed is asked to try again while the group plays. */
export const FAILED_RETRY_MS = 10_000;

/** The part of a player the group needs. TimelinePlayer implements it. */
export interface SyncPlayer {
  /** Wall-clock position, or `null` when nothing is loaded. */
  wallTime(): number | null;
  /** Whether a recording exists at a wall-clock time. */
  hasRecordingAt(wallMs: number): boolean;
  /** `true` while the player wants to play but waits for data. */
  isBuffering(): boolean;
  /** `true` when the player has video loaded at its position (playing or paused). */
  isLoaded(): boolean;
  /** `true` when the player gave up (an error, or a codec the browser cannot play). */
  hasFailed?(): boolean;
  seekWall(wallMs: number, play: boolean): void;
  play(): void;
  pause(): void;
  /** The speed the user chose. */
  setRate(rate: number): void;
  /** A small temporary correction on top of the chosen speed; 1 = none. */
  setTrim(factor: number): void;
  /** Stop and show that this camera has no recording at this moment. */
  showNoRecording(wallMs: number): void;
  step(direction: 1 | -1): void;
}

/**
 * Keeps several players on one wall clock. With one player it only forwards
 * the controls.
 */
export class SyncGroup {
  private players: SyncPlayer[] = [];
  private timer: ReturnType<typeof setInterval> | null = null;
  private playing = false;
  private rate = 1;
  private held = false;
  private bufferingSince = new Map<SyncPlayer, number>();
  private correcting = new Set<SyncPlayer>();
  private clockMs: number | null = null;
  /** When a failed player was last asked to try again. */
  private retriedAt = new Map<SyncPlayer, number>();
  private readonly now: () => number;

  constructor(now: () => number = () => Date.now()) {
    this.now = now;
  }

  setPlayers(players: SyncPlayer[]): void {
    this.players = players.slice(0, MAX_SYNC_PLAYERS);
    this.bufferingSince.clear();
    this.correcting.clear();
    this.retriedAt.clear();
  }

  /**
   * The player that drives the clock: the first one with video at its
   * position. When the first camera has stopped (its recordings ended, an
   * error), the next one takes over, so the others are never held to a
   * clock that no longer moves.
   */
  private source(): SyncPlayer | undefined {
    return this.players.find((p) => p.isLoaded() && p.wallTime() !== null);
  }

  /** The shared wall clock: the driving player's position, else the last known one. */
  wallTime(): number | null {
    const src = this.source();
    const t = src?.wallTime() ?? null;
    if (t !== null) this.clockMs = t;
    return this.clockMs;
  }

  isPlaying(): boolean {
    return this.playing;
  }

  seek(wallMs: number, play: boolean = this.playing): void {
    this.clockMs = wallMs;
    this.playing = play;
    this.held = false;
    this.bufferingSince.clear();
    for (const p of this.players) {
      p.setTrim(1);
      if (p === this.players[0] || p.hasRecordingAt(wallMs)) p.seekWall(wallMs, play);
      else p.showNoRecording(wallMs);
    }
    this.ensureTimer();
  }

  play(): void {
    this.playing = true;
    this.held = false;
    const clock = this.wallTime();
    for (const p of this.players) {
      if (p.isLoaded()) p.play();
      else if (clock !== null && (p === this.players[0] || p.hasRecordingAt(clock))) p.seekWall(clock, true);
    }
    this.ensureTimer();
  }

  pause(): void {
    this.playing = false;
    this.held = false;
    for (const p of this.players) {
      p.setTrim(1);
      p.pause();
    }
    this.correcting.clear();
  }

  setRate(rate: number): void {
    this.rate = rate;
    for (const p of this.players) p.setRate(rate);
  }

  step(direction: 1 | -1): void {
    this.pause();
    for (const p of this.players) p.step(direction);
  }

  stop(): void {
    if (this.timer !== null) clearInterval(this.timer);
    this.timer = null;
  }

  private ensureTimer(): void {
    if (this.timer === null) this.timer = setInterval(() => this.tick(), SYNC_INTERVAL_MS);
  }

  /** One correction round. Public for tests. */
  tick(): void {
    const master = this.players[0];
    if (!master || !this.playing) return;
    const now = this.now();

    // Buffering: the clock waits up to two seconds for a player that has no
    // data, then carries on and that player catches up.
    let longest: number | null = null;
    for (const p of this.players) {
      if (p.isBuffering()) {
        const since = this.bufferingSince.get(p) ?? now;
        this.bufferingSince.set(p, since);
        if (longest === null || since < longest) longest = since;
      } else {
        this.bufferingSince.delete(p);
      }
    }
    const hold = holdClock(longest, now);
    if (hold && !this.held) {
      this.held = true;
      for (const p of this.players) if (!p.isBuffering()) p.pause();
    } else if (!hold && this.held) {
      this.held = false;
      for (const p of this.players) if (p.isLoaded()) p.play();
    }
    if (this.held) return;

    const clock = this.wallTime();
    if (clock === null) return;
    const src = this.source() ?? master;
    for (const p of this.players) {
      if (p === src) continue;
      if (p.hasFailed?.()) {
        // It gave up: try again now and then, not on every round.
        const last = this.retriedAt.get(p) ?? 0;
        if (now - last < FAILED_RETRY_MS || !p.hasRecordingAt(clock)) continue;
        this.retriedAt.set(p, now);
        p.seekWall(clock, true);
        continue;
      }
      if (!p.hasRecordingAt(clock)) {
        // Nothing recorded on this camera right now: say so, and join again
        // at its next span.
        if (p.isLoaded()) p.showNoRecording(clock);
        this.correcting.delete(p);
        continue;
      }
      const own = p.wallTime();
      if (!p.isLoaded() || own === null) {
        if (!p.isBuffering()) p.seekWall(clock, true);
        continue;
      }
      if (p.isBuffering()) continue;
      const action = syncAction(clock, own, this.rate, this.correcting.has(p));
      switch (action.kind) {
        case 'seek':
          this.correcting.delete(p);
          p.setTrim(1);
          p.seekWall(action.wallMs, true);
          break;
        case 'rate':
          this.correcting.add(p);
          p.setTrim(action.rate / this.rate);
          break;
        default:
          this.correcting.delete(p);
          p.setTrim(1);
      }
    }
  }
}
