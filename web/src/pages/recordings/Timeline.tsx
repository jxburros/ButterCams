import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent, PointerEvent as ReactPointerEvent } from 'react';
import type { EventRow } from '../../api/types';
import { humanize } from '../../lib/format';
import { panView, ticks, zoomView } from '../../player/timelineLogic';
import { clockTime, continues, gapMessage } from '../../player/timelineMap';
import type { Span } from '../../player/timelineMap';
import '../../timeline.css';

export interface TimelineRow {
  cameraId: string;
  name: string;
  spans: Span[];
  events: EventRow[];
}

export interface TimeRange {
  fromMs: number;
  toMs: number;
}

interface TimelineProps {
  day: { startMs: number; endMs: number };
  rows: TimelineRow[];
  /** Wall-clock position of the player, or `null` when nothing is loaded. */
  playheadMs: number | null;
  nowMs: number;
  /** Keep the playhead in view while it moves. */
  follow: boolean;
  /** Called once when a click or drag ends: play from this moment. */
  onSeek: (ms: number) => void;
  /** Called while dragging (`null` when the drag ends), for fast scrubbing. */
  onScrub: (ms: number | null) => void;
  /** Called with the moment under the pointer, for the thumbnail. */
  onHover: (ms: number | null) => void;
  /** Picture for the hovered moment, if one is known. */
  hoverPicture: string | null;
  /** The selected range (Shift + drag). Export plugs in here in a later version. */
  selection: TimeRange | null;
  onSelection: (range: TimeRange | null) => void;
}

const AXIS_H = 26;
const ROW_H = 36;
const ROW_GAP = 6;

const COLORS = {
  ink: '#0a0a0a',
  paper: '#ffffff',
  soft: '#f4f4f1',
  line: '#e2e2dd',
  muted: '#5c5c58',
  butter: '#ffd21f',
  orange: '#ff8a1f',
  red: '#d7261e',
  blue: '#1f4bff',
};

function eventColor(event: EventRow): string {
  if (event.type === 'clock_jump') return COLORS.blue;
  if (event.severity === 'critical') return COLORS.red;
  if (event.severity === 'warning') return COLORS.orange;
  return COLORS.blue;
}

const EVENT_LABELS: Record<string, string> = {
  stream_stalled: 'Stream stalled',
  camera_unreachable: 'Camera unreachable',
  camera_recovered: 'Recording resumed',
  auth_failed: 'Sign-in to the camera failed',
  clock_jump: 'Clock change',
  refresh_failed: 'Hourly feed refresh failed',
  write_failed: 'Writing the recording failed',
  packets_dropped: 'Packets dropped',
  camera_panic: 'Recorder error',
  unsupported_codec: 'Unsupported video codec',
};

export function eventLabel(event: EventRow): string {
  const title = EVENT_LABELS[event.type] ?? humanize(event.type);
  const message = typeof event.data?.message === 'string' ? event.data.message : '';
  return message !== '' && message !== title ? `${title}: ${message}` : title;
}

/** The holes between a camera's spans inside the day. */
export function gapsOf(spans: readonly Span[], day: { startMs: number; endMs: number }): TimeRange[] {
  const out: TimeRange[] = [];
  for (let i = 1; i < spans.length; i++) {
    const prev = spans[i - 1];
    const next = spans[i];
    if (!prev || !next || prev.endMs === null || continues(prev, next)) continue;
    const fromMs = Math.max(prev.endMs, day.startMs);
    const toMs = Math.min(next.startMs, day.endMs);
    if (toMs > fromMs) out.push({ fromMs, toMs });
  }
  return out;
}

interface Hover {
  x: number;
  ms: number;
  label: string;
}

/**
 * The day as a track: recorded spans, gaps, recorder events, the hour being
 * recorded and the playhead. Click or drag to play from a moment; the wheel,
 * a pinch or the buttons zoom from the whole day down to one minute.
 */
export function Timeline(props: TimelineProps) {
  const { day, rows, playheadMs, nowMs, follow, onSeek, onScrub, onHover, hoverPicture, selection, onSelection } = props;
  const wrapRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [width, setWidth] = useState(800);
  const [view, setView] = useState(day);
  const [hover, setHover] = useState<Hover | null>(null);
  const viewRef = useRef(view);
  viewRef.current = view;
  const dayRef = useRef(day);
  dayRef.current = day;

  // A new day starts fully zoomed out.
  useEffect(() => setView(day), [day.startMs, day.endMs]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const measure = () => setWidth(Math.max(200, Math.floor(el.clientWidth)));
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const height = AXIS_H + Math.max(1, rows.length) * (ROW_H + ROW_GAP);
  const span = view.endMs - view.startMs;
  const xOf = useCallback((ms: number) => ((ms - view.startMs) / span) * width, [view.startMs, span, width]);
  const msOf = useCallback(
    (x: number) => Math.min(day.endMs, Math.max(day.startMs, view.startMs + (x / width) * span)),
    [day.startMs, day.endMs, view.startMs, span, width],
  );

  const gaps = useMemo(() => rows.map((row) => gapsOf(row.spans, day)), [rows, day]);

  // ------------------------------------------------------------- drawing
  useEffect(() => {
    const canvas = canvasRef.current;
    const ctx = canvas?.getContext('2d');
    if (!canvas || !ctx) return;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.round(width * dpr);
    canvas.height = Math.round(height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);

    // Axis.
    ctx.font = '11px "Martian Mono", ui-monospace, monospace';
    ctx.textBaseline = 'middle';
    const marks = ticks(day.startMs, view.startMs, view.endMs, Math.max(4, Math.floor(width / 90)));
    for (const tick of marks) {
      const x = Math.round(xOf(tick.ms)) + 0.5;
      ctx.strokeStyle = COLORS.line;
      ctx.beginPath();
      ctx.moveTo(x, AXIS_H - 6);
      ctx.lineTo(x, height);
      ctx.stroke();
      ctx.fillStyle = COLORS.muted;
      const w = ctx.measureText(tick.label).width;
      ctx.fillText(tick.label, Math.min(Math.max(2, x - w / 2), width - w - 2), 10);
    }

    rows.forEach((row, r) => {
      const top = AXIS_H + r * (ROW_H + ROW_GAP);
      ctx.fillStyle = COLORS.soft;
      ctx.fillRect(0, top, width, ROW_H);

      // Gaps: hatched orange.
      for (const gap of gaps[r] ?? []) {
        const x1 = Math.max(0, xOf(gap.fromMs));
        const x2 = Math.min(width, xOf(gap.toMs));
        if (x2 <= 0 || x1 >= width) continue;
        ctx.save();
        ctx.beginPath();
        ctx.rect(x1, top, Math.max(1, x2 - x1), ROW_H);
        ctx.clip();
        ctx.fillStyle = '#ffe9d1';
        ctx.fillRect(x1, top, Math.max(1, x2 - x1), ROW_H);
        ctx.strokeStyle = COLORS.orange;
        ctx.lineWidth = 1.5;
        for (let x = x1 - ROW_H; x < x2; x += 7) {
          ctx.beginPath();
          ctx.moveTo(x, top + ROW_H);
          ctx.lineTo(x + ROW_H, top);
          ctx.stroke();
        }
        ctx.restore();
      }

      // Recorded spans; the hour being recorded in butter.
      for (const s of row.spans) {
        const end = s.endMs ?? Math.max(nowMs, s.startMs);
        const x1 = Math.max(0, xOf(s.startMs));
        const x2 = Math.min(width, xOf(end));
        if (x2 <= 0 || x1 >= width) continue;
        const w = Math.max(1, x2 - x1);
        if (s.open) {
          ctx.fillStyle = COLORS.butter;
          ctx.fillRect(x1, top + 4, w, ROW_H - 8);
          ctx.strokeStyle = COLORS.ink;
          ctx.lineWidth = 2;
          ctx.strokeRect(x1 + 1, top + 5, Math.max(0, w - 2), ROW_H - 10);
        } else {
          ctx.fillStyle = COLORS.ink;
          ctx.fillRect(x1, top + 4, w, ROW_H - 8);
        }
        // A file boundary inside continuous recording: a hairline, when zoomed in enough to matter.
        if (span < 6 * 3_600_000 && x1 > 0) {
          ctx.fillStyle = COLORS.muted;
          ctx.fillRect(Math.round(x1), top + 4, 1, ROW_H - 8);
        }
      }

      // Recorder events: marks along the top edge of the row.
      for (const event of row.events) {
        const ms = Date.parse(event.ts);
        const x = xOf(ms);
        if (Number.isNaN(ms) || x < -6 || x > width + 6) continue;
        ctx.fillStyle = eventColor(event);
        ctx.beginPath();
        ctx.moveTo(x - 5, top);
        ctx.lineTo(x + 5, top);
        ctx.lineTo(x, top + 9);
        ctx.closePath();
        ctx.fill();
        ctx.fillRect(x - 1, top, 2, ROW_H);
      }

      // Camera name when more than one is shown.
      if (rows.length > 1) {
        ctx.font = '600 11px "Instrument Sans", system-ui, sans-serif';
        const w = ctx.measureText(row.name).width + 10;
        ctx.fillStyle = 'rgb(255 255 255 / 0.9)';
        ctx.fillRect(4, top + ROW_H / 2 - 8, w, 16);
        ctx.fillStyle = COLORS.ink;
        ctx.fillText(row.name, 9, top + ROW_H / 2);
      }
    });

    // The future.
    const nowX = xOf(nowMs);
    if (nowX < width) {
      ctx.fillStyle = 'rgb(255 255 255 / 0.6)';
      ctx.fillRect(Math.max(0, nowX), AXIS_H, width - Math.max(0, nowX), height - AXIS_H);
    }

    // Selection.
    if (selection) {
      const x1 = xOf(Math.min(selection.fromMs, selection.toMs));
      const x2 = xOf(Math.max(selection.fromMs, selection.toMs));
      ctx.fillStyle = 'rgb(31 75 255 / 0.18)';
      ctx.fillRect(x1, AXIS_H, x2 - x1, height - AXIS_H);
      ctx.strokeStyle = COLORS.blue;
      ctx.lineWidth = 2;
      ctx.strokeRect(x1, AXIS_H + 1, x2 - x1, height - AXIS_H - 2);
    }

    // Hover line.
    if (hover) {
      ctx.fillStyle = COLORS.muted;
      ctx.fillRect(Math.round(hover.x), AXIS_H - 6, 1, height - AXIS_H + 6);
    }

    // Playhead.
    if (playheadMs !== null) {
      const x = xOf(playheadMs);
      if (x >= -8 && x <= width + 8) {
        ctx.fillStyle = COLORS.red;
        ctx.fillRect(Math.round(x) - 1, AXIS_H - 8, 3, height - AXIS_H + 8);
        ctx.beginPath();
        ctx.moveTo(x - 7, AXIS_H - 12);
        ctx.lineTo(x + 8, AXIS_H - 12);
        ctx.lineTo(x + 0.5, AXIS_H - 2);
        ctx.closePath();
        ctx.fill();
      }
    }
  }, [width, height, view.startMs, view.endMs, rows, gaps, playheadMs, nowMs, selection, hover, day.startMs, xOf, span]);

  // Keep the playhead in view while it moves on its own.
  const interacting = useRef(false);
  useEffect(() => {
    if (!follow || playheadMs === null || interacting.current) return;
    const v = viewRef.current;
    if (playheadMs < v.startMs || playheadMs > v.endMs) {
      const len = v.endMs - v.startMs;
      setView(panView({ startMs: playheadMs - len * 0.1, endMs: playheadMs + len * 0.9 }, dayRef.current, 0));
    }
  }, [follow, playheadMs]);

  // ------------------------------------------------------------ describing
  const describe = useCallback(
    (ms: number, y: number): string => {
      const r = Math.min(rows.length - 1, Math.max(0, Math.floor((y - AXIS_H) / (ROW_H + ROW_GAP))));
      const row = rows[r];
      if (!row) return clockTime(ms);
      const prefix = rows.length > 1 ? `${row.name} · ` : '';
      const slack = (span / width) * 7;
      for (const event of row.events) {
        const at = Date.parse(event.ts);
        if (Math.abs(at - ms) <= slack) return `${prefix}${clockTime(at)} · ${eventLabel(event)}`;
      }
      for (const s of row.spans) {
        const end = s.endMs ?? Math.max(nowMs, s.startMs);
        if (ms >= s.startMs && ms < end) {
          return s.open
            ? `${prefix}${clockTime(ms)} · Recording now (since ${clockTime(s.startMs)})`
            : `${prefix}${clockTime(ms)} · Recorded ${clockTime(s.startMs)} – ${clockTime(end)}`;
        }
      }
      for (const gap of gaps[r] ?? []) {
        if (ms >= gap.fromMs && ms < gap.toMs) return `${prefix}${gapMessage(gap.fromMs, gap.toMs)}`;
      }
      return `${prefix}${clockTime(ms)} · No recording`;
    },
    [rows, gaps, span, width, nowMs],
  );

  // ----------------------------------------------------------- interaction
  const pointers = useRef(new Map<number, { x: number; y: number }>());
  const drag = useRef<{ mode: 'scrub' | 'select' | 'pinch'; startMs: number; pinchDist: number; pinchMid: number } | null>(null);

  function local(event: { clientX: number; clientY: number }): { x: number; y: number } {
    const rect = canvasRef.current?.getBoundingClientRect();
    return { x: event.clientX - (rect?.left ?? 0), y: event.clientY - (rect?.top ?? 0) };
  }

  function onPointerDown(event: ReactPointerEvent<HTMLCanvasElement>) {
    if (event.pointerType === 'mouse' && event.button !== 0) return;
    const p = local(event);
    pointers.current.set(event.pointerId, p);
    event.currentTarget.setPointerCapture(event.pointerId);
    interacting.current = true;
    if (pointers.current.size === 2) {
      // Two fingers: pinch to zoom, move to pan. Whatever one finger started is dropped.
      if (drag.current?.mode === 'scrub') onScrub(null);
      const [a, b] = [...pointers.current.values()] as [{ x: number; y: number }, { x: number; y: number }];
      drag.current = { mode: 'pinch', startMs: 0, pinchDist: Math.max(10, Math.abs(a.x - b.x)), pinchMid: (a.x + b.x) / 2 };
      setHover(null);
      onHover(null);
      return;
    }
    const ms = msOf(p.x);
    if (event.shiftKey) {
      drag.current = { mode: 'select', startMs: ms, pinchDist: 0, pinchMid: 0 };
      onSelection({ fromMs: ms, toMs: ms });
    } else {
      drag.current = { mode: 'scrub', startMs: ms, pinchDist: 0, pinchMid: 0 };
      onScrub(ms);
    }
  }

  function onPointerMove(event: ReactPointerEvent<HTMLCanvasElement>) {
    const p = local(event);
    if (pointers.current.has(event.pointerId)) pointers.current.set(event.pointerId, p);
    const d = drag.current;
    if (d?.mode === 'pinch' && pointers.current.size >= 2) {
      const [a, b] = [...pointers.current.values()] as [{ x: number; y: number }, { x: number; y: number }];
      const dist = Math.max(10, Math.abs(a.x - b.x));
      const mid = (a.x + b.x) / 2;
      const v = viewRef.current;
      const len = v.endMs - v.startMs;
      const around = v.startMs + (d.pinchMid / width) * len;
      let next = zoomView(v, dayRef.current, d.pinchDist / dist, around);
      next = panView(next, dayRef.current, (-(mid - d.pinchMid) / width) * (next.endMs - next.startMs));
      d.pinchDist = dist;
      d.pinchMid = mid;
      setView(next);
      return;
    }
    const ms = msOf(p.x);
    setHover({ x: p.x, ms, label: describe(ms, p.y) });
    onHover(ms);
    if (d?.mode === 'select') onSelection({ fromMs: d.startMs, toMs: ms });
    else if (d?.mode === 'scrub') onScrub(ms);
  }

  function endPointer(event: ReactPointerEvent<HTMLCanvasElement>, cancelled: boolean) {
    const had = pointers.current.delete(event.pointerId);
    const d = drag.current;
    if (!had || !d) return;
    if (d.mode === 'pinch') {
      if (pointers.current.size === 0) {
        drag.current = null;
        interacting.current = false;
      }
      return;
    }
    drag.current = null;
    interacting.current = false;
    const ms = msOf(local(event).x);
    if (d.mode === 'select') {
      const range = { fromMs: Math.min(d.startMs, ms), toMs: Math.max(d.startMs, ms) };
      onSelection(cancelled || range.toMs - range.fromMs < 1000 ? null : range);
      return;
    }
    onScrub(null);
    if (!cancelled) onSeek(ms);
    if (event.pointerType !== 'mouse') {
      setHover(null);
      onHover(null);
    }
  }

  // The wheel zooms around the pointer; sideways (or with Shift) it pans.
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      const v = viewRef.current;
      const len = v.endMs - v.startMs;
      const rect = canvas.getBoundingClientRect();
      if (Math.abs(event.deltaX) > Math.abs(event.deltaY) || event.shiftKey) {
        const delta = Math.abs(event.deltaX) > Math.abs(event.deltaY) ? event.deltaX : event.deltaY;
        setView(panView(v, dayRef.current, (delta / rect.width) * len));
        return;
      }
      const around = v.startMs + ((event.clientX - rect.left) / rect.width) * len;
      setView(zoomView(v, dayRef.current, event.deltaY > 0 ? 1.25 : 0.8, around));
    };
    canvas.addEventListener('wheel', onWheel, { passive: false });
    return () => canvas.removeEventListener('wheel', onWheel);
  }, []);

  function zoom(factor: number) {
    const v = viewRef.current;
    const around = playheadMs !== null && playheadMs >= v.startMs && playheadMs <= v.endMs ? playheadMs : (v.startMs + v.endMs) / 2;
    setView(zoomView(v, day, factor, around));
  }

  function onKeyDown(event: ReactKeyboardEvent<HTMLDivElement>) {
    const base = playheadMs ?? view.startMs;
    const stepMs = event.shiftKey ? 60_000 : 5000;
    switch (event.key) {
      case 'ArrowLeft':
        onSeek(Math.max(day.startMs, base - stepMs));
        break;
      case 'ArrowRight':
        onSeek(Math.min(day.endMs, base + stepMs));
        break;
      case 'Home':
        onSeek(day.startMs);
        break;
      case '+':
      case '=':
        zoom(0.5);
        break;
      case '-':
        zoom(2);
        break;
      default:
        return;
    }
    event.preventDefault();
  }

  const zoomedOut = view.startMs <= day.startMs && view.endMs >= day.endMs;
  const tipLeft = hover ? Math.min(Math.max(hover.x, 90), width - 90) : 0;

  return (
    <div className="tl">
      <div className="tl-tools">
        <span className="tl-range mono">
          {clockTime(view.startMs)} – {clockTime(view.endMs - 1)}
        </span>
        <div className="button-row">
          <button type="button" className="button button-small" onClick={() => setView(panView(view, day, -span / 2))} disabled={view.startMs <= day.startMs} aria-label="Earlier">
            ◀
          </button>
          <button type="button" className="button button-small" onClick={() => zoom(2)} disabled={zoomedOut} aria-label="Zoom out">
            −
          </button>
          <button type="button" className="button button-small" onClick={() => zoom(0.5)} disabled={span <= 60_000} aria-label="Zoom in">
            +
          </button>
          <button type="button" className="button button-small" onClick={() => setView(day)} disabled={zoomedOut}>
            Whole day
          </button>
          <button type="button" className="button button-small" onClick={() => setView(panView(view, day, span / 2))} disabled={view.endMs >= day.endMs} aria-label="Later">
            ▶
          </button>
        </div>
      </div>
      <div
        ref={wrapRef}
        className="tl-track"
        tabIndex={0}
        role="slider"
        aria-label="Timeline of the day. Left and right arrows move 5 seconds, with Shift one minute; plus and minus zoom."
        aria-valuemin={Math.round(day.startMs / 1000)}
        aria-valuemax={Math.round(day.endMs / 1000)}
        aria-valuenow={Math.round((playheadMs ?? day.startMs) / 1000)}
        aria-valuetext={playheadMs === null ? 'Nothing is playing' : describe(playheadMs, AXIS_H + 1)}
        onKeyDown={onKeyDown}
      >
        <canvas
          ref={canvasRef}
          style={{ width: '100%', height }}
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={(event) => endPointer(event, false)}
          onPointerCancel={(event) => endPointer(event, true)}
          onPointerLeave={() => {
            if (!drag.current) {
              setHover(null);
              onHover(null);
            }
          }}
        />
        {hover ? (
          <div className="tl-tip" style={{ left: tipLeft }} aria-hidden="true">
            {hoverPicture ? <img src={hoverPicture} alt="" width={160} /> : null}
            <span>{hover.label}</span>
          </div>
        ) : null}
      </div>
      <ul className="legend tl-legend">
        <li>Recorded</li>
        <li className="tl-key-now">Recording now</li>
        <li className="tl-key-gap">No recording</li>
        <li className="tl-key-event">Recorder event</li>
        <li className="tl-key-clock">Clock change</li>
      </ul>
    </div>
  );
}
