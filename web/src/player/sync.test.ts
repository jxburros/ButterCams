import { describe, expect, it } from 'vitest';
import { holdClock, SyncGroup, syncAction } from './SyncGroup';
import type { SyncPlayer } from './SyncGroup';

describe('sync decision', () => {
  it('leaves a follower alone within 150 ms', () => {
    expect(syncAction(10_000, 10_100, 1, false)).toEqual({ kind: 'none', rate: 1 });
    expect(syncAction(10_000, 9_850, 2, false)).toEqual({ kind: 'none', rate: 2 });
  });

  it('changes speed by 5% when off by more than 150 ms', () => {
    expect(syncAction(10_000, 9_800, 1, false)).toEqual({ kind: 'rate', rate: 1.05 }); // behind: faster
    expect(syncAction(10_000, 10_200, 1, false)).toEqual({ kind: 'rate', rate: 0.95 }); // ahead: slower
    expect(syncAction(10_000, 9_800, 4, false)).toEqual({ kind: 'rate', rate: 4.2 });
  });

  it('keeps correcting until well inside the tolerance', () => {
    expect(syncAction(10_000, 9_900, 1, true).kind).toBe('rate');
    expect(syncAction(10_000, 9_970, 1, true).kind).toBe('none');
  });

  it('seeks when off by more than a second', () => {
    expect(syncAction(10_000, 8_900, 1, false)).toEqual({ kind: 'seek', wallMs: 10_000 });
    expect(syncAction(10_000, 11_500, 1, true)).toEqual({ kind: 'seek', wallMs: 10_000 });
    expect(syncAction(10_000, 9_000, 1, false).kind).toBe('rate'); // exactly one second is not "more"
  });

  it('holds the clock for a buffering player for at most two seconds', () => {
    expect(holdClock(null, 5000)).toBe(false);
    expect(holdClock(4000, 5000)).toBe(true);
    expect(holdClock(3001, 5000)).toBe(true);
    expect(holdClock(3000, 5000)).toBe(false);
  });
});

class FakePlayer implements SyncPlayer {
  wall: number | null = null;
  loaded = false;
  buffering = false;
  playing = false;
  trim = 1;
  rate = 1;
  from = 0;
  to = Infinity;
  log: string[] = [];

  wallTime() {
    return this.wall;
  }
  hasRecordingAt(wallMs: number) {
    return wallMs >= this.from && wallMs < this.to;
  }
  isBuffering() {
    return this.buffering;
  }
  isLoaded() {
    return this.loaded;
  }
  seekWall(wallMs: number, play: boolean) {
    this.log.push(`seek ${wallMs}`);
    this.wall = wallMs;
    this.loaded = true;
    this.playing = play;
  }
  play() {
    this.log.push('play');
    this.playing = true;
  }
  pause() {
    this.log.push('pause');
    this.playing = false;
  }
  setRate(rate: number) {
    this.rate = rate;
  }
  setTrim(factor: number) {
    this.trim = factor;
  }
  showNoRecording(wallMs: number) {
    this.log.push(`none ${wallMs}`);
    this.loaded = false;
    this.playing = false;
    this.wall = wallMs;
  }
  step(direction: 1 | -1) {
    this.log.push(`step ${direction}`);
  }
}

describe('sync group', () => {
  function group(n: number) {
    let now = 0;
    const g = new SyncGroup(() => now);
    const players = Array.from({ length: n }, () => new FakePlayer());
    g.setPlayers(players);
    return { g, players, advance: (ms: number) => (now += ms), stop: () => g.stop() };
  }

  it('starts every camera at the same moment and corrects followers', () => {
    const { g, players, stop } = group(3);
    const [master, b, c] = players as [FakePlayer, FakePlayer, FakePlayer];
    g.seek(50_000, true);
    expect(players.every((p) => p.wall === 50_000 && p.playing)).toBe(true);

    master.wall = 60_000;
    b.wall = 59_800; // 200 ms behind
    c.wall = 60_050; // fine
    g.tick();
    expect(b.trim).toBeCloseTo(1.05, 9);
    expect(c.trim).toBe(1);

    b.wall = 59_990; // caught up
    c.wall = 57_000; // far behind
    g.tick();
    expect(b.trim).toBe(1);
    expect(c.log.at(-1)).toBe('seek 60000');
    stop();
  });

  it('waits up to two seconds for a buffering camera, then carries on', () => {
    const { g, players, advance, stop } = group(2);
    const [master, b] = players as [FakePlayer, FakePlayer];
    g.seek(0, true);
    master.wall = 1000;
    b.wall = 1000;
    b.buffering = true;
    g.tick();
    expect(master.playing).toBe(false); // the clock holds
    advance(1900);
    g.tick();
    expect(master.playing).toBe(false);
    advance(200);
    g.tick();
    expect(master.playing).toBe(true); // the others continue
    // When it has data again it is behind and seeks to the clock.
    master.wall = 5000;
    b.buffering = false;
    g.tick();
    expect(b.log.at(-1)).toBe('seek 5000');
    stop();
  });

  it('shows "no recording" on a camera without one and joins again at its next span', () => {
    const { g, players, stop } = group(2);
    const [master, b] = players as [FakePlayer, FakePlayer];
    b.from = 10_000;
    g.seek(5_000, true);
    expect(master.log).toContain('seek 5000');
    expect(b.log).toEqual(['none 5000']);
    master.wall = 9_000;
    g.tick();
    expect(b.loaded).toBe(false);
    master.wall = 10_050;
    g.tick();
    expect(b.log.at(-1)).toBe('seek 10050');
    // And it leaves again when its recording ends.
    b.to = 20_000;
    master.wall = 20_500;
    g.tick();
    expect(b.log.at(-1)).toBe('none 20500');
    stop();
  });

  it('forwards the controls to every camera', () => {
    const { g, players, stop } = group(4);
    g.seek(1000, false);
    g.setRate(8);
    expect(players.map((p) => p.rate)).toEqual([8, 8, 8, 8]);
    g.play();
    expect(players.every((p) => p.playing)).toBe(true);
    g.step(1);
    expect(players.every((p) => !p.playing && p.log.at(-1) === 'step 1')).toBe(true);
    expect(g.isPlaying()).toBe(false);
    stop();
  });

  it('takes at most four cameras', () => {
    const g = new SyncGroup();
    const players = Array.from({ length: 6 }, () => new FakePlayer());
    g.setPlayers(players);
    g.seek(1, false);
    expect(players.filter((p) => p.log.length > 0)).toHaveLength(4);
    g.stop();
  });
});
