import { describe, expect, it } from 'vitest';
import type { StreamKind } from '../api/types';
import {
  BACKOFF_MS,
  backoffDelay,
  holeSkip,
  codecLabel,
  liveEdgeAction,
  parseLiveMessage,
  pickStream,
  probeUrl,
  trimRange,
  unsupportedMessage,
} from './liveLogic';

describe('backoff', () => {
  it('waits 1, 2, 5, 10 s and then stays at 10 s', () => {
    expect([0, 1, 2, 3, 4, 50].map(backoffDelay)).toEqual([1000, 2000, 5000, 10000, 10000, 10000]);
    expect(BACKOFF_MS).toEqual([1000, 2000, 5000, 10000]);
    expect(backoffDelay(-1)).toBe(1000);
  });
});

describe('live edge', () => {
  it('plays at normal speed close to the edge', () => {
    expect(liveEdgeAction(10, 10.3, 1)).toEqual({ kind: 'rate', rate: 1 });
  });
  it('speeds up when more than 1 s behind', () => {
    expect(liveEdgeAction(10, 11.5, 1)).toEqual({ kind: 'rate', rate: 1.1 });
  });
  it('keeps the faster rate until it is close again', () => {
    expect(liveEdgeAction(10, 10.8, 1.1)).toEqual({ kind: 'rate', rate: 1.1 });
    expect(liveEdgeAction(10, 10.4, 1.1)).toEqual({ kind: 'rate', rate: 1 });
  });
  it('jumps when more than 3 s behind', () => {
    expect(liveEdgeAction(10, 14, 1)).toEqual({ kind: 'seek', to: 13.8 });
  });
  it('never seeks before zero', () => {
    expect(liveEdgeAction(-5, 0.1, 1)).toEqual({ kind: 'seek', to: 0 });
  });
});

describe('buffer trimming', () => {
  it('leaves a short buffer alone', () => {
    expect(trimRange(0, 13)).toBeNull();
  });
  it('removes everything older than 10 s once enough has piled up', () => {
    expect(trimRange(0, 20)).toEqual([0, 10]);
    expect(trimRange(100, 114)).toEqual([100, 104]);
  });
});

describe('messages', () => {
  it('reads init', () => {
    expect(parseLiveMessage('{"type":"init","mime":"video/mp4; codecs=\\"avc1.64001f\\"","width":640,"height":360}')).toEqual({
      type: 'init',
      mime: 'video/mp4; codecs="avc1.64001f"',
      width: 640,
      height: 360,
    });
  });
  it('reads state', () => {
    expect(parseLiveMessage('{"type":"state","state":"waiting","message":"Camera offline"}')).toEqual({
      type: 'state',
      state: 'waiting',
      message: 'Camera offline',
    });
  });
  it('ignores anything else', () => {
    expect(parseLiveMessage('nonsense')).toBeNull();
    expect(parseLiveMessage('{"type":"init"}')).toBeNull();
    expect(parseLiveMessage('{"type":"later"}')).toBeNull();
    expect(parseLiveMessage('null')).toBeNull();
  });
});

describe('stream choice', () => {
  const none = new Set<StreamKind>();
  it('uses the sub stream in the grid and the main stream in single view', () => {
    expect(pickStream({ mode: 'tile', hasSub: true, mainEnabled: true, unsupported: none })).toBe('sub');
    expect(pickStream({ mode: 'single', hasSub: true, mainEnabled: true, unsupported: none })).toBe('main');
  });
  it('uses the main stream in the grid for a camera without a sub stream', () => {
    expect(pickStream({ mode: 'tile', hasSub: false, mainEnabled: true, unsupported: none })).toBe('main');
  });
  it('falls back to the sub stream when the main codec cannot be played', () => {
    expect(pickStream({ mode: 'single', hasSub: true, mainEnabled: true, unsupported: new Set<StreamKind>(['main']) })).toBe('sub');
  });
  it('uses sub streams only when the main stream is switched off', () => {
    expect(pickStream({ mode: 'single', hasSub: true, mainEnabled: false, unsupported: none })).toBe('sub');
    expect(pickStream({ mode: 'tile', hasSub: false, mainEnabled: false, unsupported: none })).toBeNull();
  });
  it('gives up when nothing can be played', () => {
    expect(
      pickStream({ mode: 'tile', hasSub: true, mainEnabled: true, unsupported: new Set<StreamKind>(['main', 'sub']) }),
    ).toBeNull();
  });
});

describe('texts', () => {
  it('names the codec', () => {
    expect(codecLabel('video/mp4; codecs="hvc1.1.6.L93.B0,mp4a.40.2"')).toBe('H.265');
    expect(codecLabel('video/mp4; codecs="avc1.64001f"')).toBe('H.264');
    expect(unsupportedMessage('video/mp4; codecs="hvc1.1.6.L93.B0"')).toContain('H.265');
  });
  it('turns the WebSocket address into the HTTP one', () => {
    expect(probeUrl('wss://nvr:8650/api/v1/live/a/sub?audio=0')).toBe('https://nvr:8650/api/v1/live/a/sub?audio=0');
    expect(probeUrl('ws://localhost/api/v1/live/a/main')).toBe('http://localhost/api/v1/live/a/main');
  });
});

describe('holes in the live buffer', () => {
  it('skips a hole the playhead stands at', () => {
    expect(holeSkip([[0, 446.9], [447.4, 450]], 446.88)).toBeCloseTo(447.41, 5);
    expect(holeSkip([[0, 446.9], [447.4, 450]], 447.0)).toBeCloseTo(447.41, 5);
  });
  it('leaves playback alone inside a range or without a later one', () => {
    expect(holeSkip([[0, 446.9], [447.4, 450]], 440)).toBeNull();
    expect(holeSkip([[0, 450]], 449.95)).toBeNull();
  });
});
