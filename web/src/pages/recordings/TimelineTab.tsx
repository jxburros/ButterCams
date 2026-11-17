import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent } from 'react';
import { api, errorMessage } from '../../api/client';
import type { EventRow, TimelineSegment } from '../../api/types';
import { Alert } from '../../components/Alert';
import { SelectField, TextField } from '../../components/Fields';
import { Icon } from '../../components/Icon';
import { Segmented } from '../../components/PageHeader';
import { todayLocal } from '../../lib/format';
import { useLoad, useNow } from '../../lib/hooks';
import { MAX_SYNC_PLAYERS, SyncGroup } from '../../player/SyncGroup';
import { Thumbnails } from '../../player/thumbnails';
import { audioAllowed, dateOf, dayBounds, formatOffset, offsetOf, shiftDate, SPEEDS } from '../../player/timelineLogic';
import { clockTime, seekTarget } from '../../player/timelineMap';
import type { Span } from '../../player/timelineMap';
import { TimelinePlayer } from '../../player/TimelinePlayer';
import type { PlayerState } from '../../player/TimelinePlayer';
import { eventLabel, gapsOf, Timeline } from './Timeline';
import type { TimelineRow, TimeRange } from './Timeline';
import '../../timeline.css';

interface CameraData {
  spans: Span[];
  events: EventRow[];
}

/** How far past midnight spans are loaded, so playback runs on into the next day. */
const AFTER_DAY_MS = 2 * 3_600_000;
const REFRESH_MS = 10_000;

function toSpan(segment: TimelineSegment): Span | null {
  const startMs = Date.parse(segment.start);
  if (Number.isNaN(startMs)) return null;
  const endMs = segment.end === null ? null : Date.parse(segment.end);
  return {
    id: segment.id,
    startMs,
    endMs: endMs === null || Number.isNaN(endMs) ? null : endMs,
    gapBefore: segment.gapBefore,
    open: segment.open,
    videoCodec: segment.videoCodec,
    fileName: segment.fileName,
  };
}

function Glyph({ d, fill }: { d: string; fill?: boolean }) {
  return (
    <svg className="icon" aria-hidden="true" focusable="false" width={20} height={20} viewBox="0 0 24 24" fill={fill ? 'currentColor' : 'none'} stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
      <path d={d} />
    </svg>
  );
}

const GLYPHS = {
  play: 'M7 4l13 8-13 8z',
  pause: 'M7 4v16M17 4v16',
  stepBack: 'M18 5v14l-10-7zM6 5v14',
  stepForward: 'M6 5v14l10-7zM18 5v14',
  sound: 'M4 9v6h4l5 4V5L8 9zM16 9a4 4 0 0 1 0 6M18.5 6.5a8 8 0 0 1 0 11',
  muted: 'M4 9v6h4l5 4V5L8 9zM16 9l5 6M21 9l-5 6',
};

const STATUS_TEXT: Partial<Record<PlayerState['status'], string>> = {
  loading: 'Loading…',
  buffering: 'Loading…',
};

function PlayerTile({
  cameraId,
  name,
  fps,
  spans,
  state,
  showName,
  onPlayer,
  onState,
  onWantSpans,
}: {
  cameraId: string;
  name: string;
  fps: number | undefined;
  spans: Span[];
  state: PlayerState | undefined;
  showName: boolean;
  onPlayer: (cameraId: string, player: TimelinePlayer | null) => void;
  onState: (cameraId: string, state: PlayerState) => void;
  onWantSpans: () => void;
}) {
  const videoRef = useRef<HTMLVideoElement>(null);
  const playerRef = useRef<TimelinePlayer | null>(null);
  const spansRef = useRef(spans);
  spansRef.current = spans;

  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    const player = new TimelinePlayer(video, { fps, onState: (s) => onState(cameraId, s), onWantSpans });
    player.setSpans(spansRef.current);
    playerRef.current = player;
    onPlayer(cameraId, player);
    return () => {
      onPlayer(cameraId, null);
      playerRef.current = null;
      player.destroy();
    };
    // The frame rate is only a fallback for frame steps; a new value does not justify a new player.
  }, [cameraId, onPlayer, onState, onWantSpans]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    playerRef.current?.setSpans(spans);
  }, [spans]);

  const status = state?.status ?? 'idle';
  const blocking = status === 'idle' || status === 'norecording' || status === 'unsupported' || status === 'error' || status === 'ended';
  const span = state?.segmentId != null ? spans.find((s) => s.id === state.segmentId) : undefined;
  let text = state?.message ?? STATUS_TEXT[status] ?? null;
  if (status === 'idle') text = 'Click the timeline to play from that moment.';

  return (
    <div className={`tlp-tile tlp-${status}`}>
      <video ref={videoRef} className="tlp-video" playsInline muted aria-label={`Recording of ${name}`} />
      {showName ? <span className="tlp-name">{name}</span> : null}
      {state?.keyframes && !blocking ? <span className="tlp-badge">Keyframes only</span> : null}
      {state?.atLiveEdge && !blocking ? <span className="tlp-badge tlp-badge-live">Latest recorded video</span> : null}
      {text ? (
        <div className={blocking ? 'tlp-message tlp-message-block' : 'tlp-message'} role="status">
          {blocking ? <Icon name={status === 'error' || status === 'unsupported' ? 'cameras' : 'clock'} size={40} /> : null}
          <p>{text}</p>
          {(status === 'unsupported' || status === 'error') && span && !span.open ? (
            <a className="button" href={api.segmentDownloadUrl(span.id)} download={span.fileName}>
              <Icon name="download" size={18} />
              Download this hour
            </a>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

/** The timeline that plays: up to four cameras on one clock (spec 16.5). */
export function TimelineTab() {
  const cameras = useLoad(api.recordingCameras);
  const liveCameras = useLoad(api.cameras);
  const [picked, setPicked] = useState<string[]>(['']);
  const [date, setDate] = useState(todayLocal);
  const [data, setData] = useState<Record<string, CameraData>>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [serverOffset, setServerOffset] = useState<number | null>(null);
  const [states, setStates] = useState<Record<string, PlayerState>>({});
  const [wantPlaying, setPlaying] = useState(false);
  const [rate, setRate] = useState(1);
  const [muted, setMuted] = useState(true);
  const [selection, setSelection] = useState<TimeRange | null>(null);
  const [hoverPicture, setHoverPicture] = useState<string | null>(null);
  const [playerVersion, setPlayerVersion] = useState(0);
  const now = useNow();

  const cameraList = cameras.data?.cameras ?? [];
  const selected = useMemo(() => {
    const out: string[] = [];
    for (const id of picked) if (id !== '' && !out.includes(id)) out.push(id);
    return out.slice(0, MAX_SYNC_PLAYERS);
  }, [picked]);
  const selectedKey = selected.join('|');

  // Start with the first camera so the page shows something at once.
  useEffect(() => {
    const first = cameraList.find((camera) => !camera.deleted) ?? cameraList[0];
    if (first) setPicked((current) => (current.length === 1 && current[0] === '' ? [first.id] : current));
  }, [cameraList.length]); // eslint-disable-line react-hooks/exhaustive-deps

  const day = useMemo(() => dayBounds(date), [date]);
  const dateError = day ? undefined : 'Choose a date.';
  const isToday = day !== null && now >= day.startMs && now < day.endMs + AFTER_DAY_MS;

  // ------------------------------------------------------------- loading
  /** Set while the date changes because playback crossed midnight. */
  const autoDayRef = useRef(false);
  /** Loads the timeline again now (a player reached the newest file it knows). */
  const reloadRef = useRef<(() => void) | null>(null);
  const wantedAt = useRef(0);
  const onWantSpans = useCallback(() => {
    const now = Date.now();
    if (now - wantedAt.current < 2000) return; // one request for all players
    wantedAt.current = now;
    reloadRef.current?.();
  }, []);
  useEffect(() => {
    if (!day || selected.length === 0) {
      setData({});
      setFailure(null);
      return;
    }
    let current = true;
    const load = () => {
      void Promise.all(
        selected.map(async (id) => {
          const response = await api.timeline(id, new Date(day.startMs), new Date(day.endMs + AFTER_DAY_MS));
          const spans = response.segments.map(toSpan).filter((s): s is Span => s !== null);
          return [id, { spans, events: response.events }, offsetOf(response.now)] as const;
        }),
      )
        .then((results) => {
          if (!current) return;
          const next: Record<string, CameraData> = {};
          for (const [id, cameraData] of results) next[id] = cameraData;
          setData(next);
          setServerOffset(results[0]?.[2] ?? null);
          setFailure(null);
        })
        .catch((err: unknown) => {
          if (current) setFailure(errorMessage(err));
        });
    };
    // When playback itself carried the page into the next day, the players
    // keep what they have until the new day's list arrives.
    if (!autoDayRef.current) setData({});
    load();
    reloadRef.current = load;
    // The day that is still being recorded changes: new files, closed files.
    const timer = isToday ? window.setInterval(load, REFRESH_MS) : null;
    return () => {
      current = false;
      if (reloadRef.current === load) reloadRef.current = null;
      if (timer !== null) window.clearInterval(timer);
    };
  }, [selectedKey, day?.startMs, day?.endMs, isToday]); // eslint-disable-line react-hooks/exhaustive-deps

  // ------------------------------------------------------------- players
  const players = useRef(new Map<string, TimelinePlayer>());
  const groupRef = useRef<SyncGroup | null>(null);
  if (groupRef.current === null) groupRef.current = new SyncGroup();
  const group = groupRef.current;
  useEffect(() => () => group.stop(), [group]);

  const onPlayer = useCallback((cameraId: string, player: TimelinePlayer | null) => {
    if (player) players.current.set(cameraId, player);
    else players.current.delete(cameraId);
    setPlayerVersion((v) => v + 1);
  }, []);
  const onState = useCallback((cameraId: string, state: PlayerState) => {
    setStates((current) => ({ ...current, [cameraId]: state }));
  }, []);

  useEffect(() => {
    const list = selected.map((id) => players.current.get(id)).filter((p): p is TimelinePlayer => p !== undefined);
    group.setPlayers(list);
    for (const p of list) {
      p.setRate(rate);
      p.setMuted(muted);
    }
    setStates((current) => {
      const next: Record<string, PlayerState> = {};
      for (const id of selected) if (current[id]) next[id] = current[id];
      return next;
    });
  }, [selectedKey, playerVersion]); // eslint-disable-line react-hooks/exhaustive-deps

  const masterId = selected[0];
  const master = masterId ? states[masterId] : undefined;
  const isStopped = (s: PlayerState | undefined) =>
    s?.status === 'ended' || s?.status === 'error' || s?.status === 'unsupported' || s?.status === 'norecording';
  // The clock follows the first camera that has video (as the sync group
  // does): when the first one stops, the others carry on.
  const driver = selected.map((id) => states[id]).find((s) => s !== undefined && !isStopped(s) && s.wallMs !== null) ?? master;
  const playheadMs = driver?.wallMs ?? null;
  // Nothing is playing once every camera has stopped.
  const stopped = selected.length > 0 && selected.every((id) => isStopped(states[id]));
  const playing = wantPlaying && !stopped;
  const loaded = Object.keys(data).length > 0;

  // A new day or camera set: stop, nothing is at the playhead any more.
  useEffect(() => {
    if (autoDayRef.current) {
      // Playback carried the page into the next day: it goes on playing.
      autoDayRef.current = false;
      return;
    }
    group.pause();
    setPlaying(false);
    setSelection(null);
  }, [date, selectedKey, group]);

  // Playback that runs past midnight takes the page to the next day.
  useEffect(() => {
    if (playing && day && playheadMs !== null && playheadMs >= day.endMs) {
      autoDayRef.current = true;
      setDate(dateOf(playheadMs));
    }
  }, [playing, playheadMs, day]);


  // ------------------------------------------------------------ controls
  const seek = useCallback(
    (ms: number, play: boolean) => {
      if (!loaded) return;
      group.seek(ms, play);
      setPlaying(play);
    },
    [group, loaded],
  );

  function togglePlay() {
    if (!loaded) return;
    if (playing) {
      group.pause();
      setPlaying(false);
      return;
    }
    if (playheadMs === null || stopped) {
      // Nothing chosen yet: start at the first recording of the day.
      const first = masterId ? data[masterId]?.spans[0] : undefined;
      if (first && day) seek(Math.max(first.startMs, day.startMs), true);
      return;
    }
    group.play();
    setPlaying(true);
  }

  function changeRate(next: number) {
    setRate(next);
    group.setRate(next);
  }

  function changeMuted(next: boolean) {
    setMuted(next);
    for (const p of players.current.values()) p.setMuted(next);
  }

  function step(direction: 1 | -1) {
    group.step(direction);
    setPlaying(false);
  }

  // Scrubbing: keyframes only while the pointer drags, a normal seek on release.
  const scrub = useRef<{ startMs: number; active: boolean; last: number } | null>(null);
  const onScrub = useCallback(
    (ms: number | null) => {
      if (ms === null) {
        if (scrub.current?.active) for (const p of players.current.values()) p.setScrubbing(false);
        scrub.current = null;
        return;
      }
      if (scrub.current === null) {
        scrub.current = { startMs: ms, active: false, last: 0 };
        return; // a plain click never enters scrub mode
      }
      const s = scrub.current;
      if (!s.active) {
        if (ms === s.startMs || !loaded) return;
        s.active = true;
        for (const p of players.current.values()) p.setScrubbing(true);
      }
      const t = performance.now();
      if (t - s.last < 150) return;
      s.last = t;
      group.seek(ms, false);
      setPlaying(false);
    },
    [group, loaded],
  );

  // Thumbnail of the first camera under the pointer.
  const thumbs = useRef<Thumbnails | null>(null);
  if (thumbs.current === null) thumbs.current = new Thumbnails();
  const hoverToken = useRef(0);
  const onHover = useCallback(
    (ms: number | null) => {
      const token = ++hoverToken.current;
      const spans = masterId ? data[masterId]?.spans : undefined;
      if (ms === null || !spans) {
        setHoverPicture(null);
        return;
      }
      const target = seekTarget(spans, ms, Date.now());
      if (target.kind !== 'inside') {
        setHoverPicture(null);
        return;
      }
      const known = thumbs.current?.cached(target.span.id, target.offsetMs) ?? null;
      setHoverPicture(known);
      if (known === null) {
        thumbs.current?.request(
          target.span.id,
          target.offsetMs,
          (picture) => {
            if (token === hoverToken.current) setHoverPicture(picture);
          },
          target.span.open,
        );
      }
    },
    [data, masterId],
  );

  const gridRef = useRef<HTMLDivElement>(null);
  function fullScreen() {
    void gridRef.current?.requestFullscreen?.().catch(() => undefined);
  }

  function onKeyDown(event: ReactKeyboardEvent<HTMLElement>) {
    const target = event.target as HTMLElement;
    const tag = target.tagName;
    if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA' || tag === 'A') return;
    if (event.key === ' ' && tag !== 'BUTTON') {
      event.preventDefault();
      togglePlay();
    } else if (event.key === ',') {
      event.preventDefault();
      step(-1);
    } else if (event.key === '.') {
      event.preventDefault();
      step(1);
    }
  }

  // ------------------------------------------------------------- derived
  const names = new Map(cameraList.map((camera) => [camera.id, camera.deleted ? `${camera.name} (deleted camera)` : camera.name]));
  const fpsOf = new Map((liveCameras.data?.cameras ?? []).map((camera) => [camera.id, camera.stats?.fps]));
  const rows: TimelineRow[] = selected.map((id) => ({
    cameraId: id,
    name: names.get(id) ?? id,
    spans: data[id]?.spans ?? [],
    events: data[id]?.events ?? [],
  }));
  const anyRecording = rows.some((row) => row.spans.length > 0);
  const currentSpan = masterId && master?.segmentId != null ? data[masterId]?.spans.find((s) => s.id === master.segmentId) : undefined;
  const browserOffset = playheadMs !== null || day ? -new Date(playheadMs ?? day?.startMs ?? 0).getTimezoneOffset() : null;
  const otherZone = serverOffset !== null && browserOffset !== null && serverOffset !== browserOffset;

  const notes = useMemo(() => {
    if (!day) return [];
    const out: { ms: number; text: string; camera: string }[] = [];
    for (const row of rows) {
      for (const gap of gapsOf(row.spans, day)) {
        out.push({ ms: gap.toMs, camera: row.name, text: `No recording ${clockTime(gap.fromMs)} – ${clockTime(gap.toMs)}` });
      }
      for (const event of row.events) {
        const ms = Date.parse(event.ts);
        if (!Number.isNaN(ms) && ms >= day.startMs && ms < day.endMs) out.push({ ms, camera: row.name, text: `${clockTime(ms)} · ${eventLabel(event)}` });
      }
    }
    return out.sort((a, b) => a.ms - b.ms).slice(0, 300);
  }, [data, selectedKey, day?.startMs]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <section className="tlp" aria-label="Timeline playback" onKeyDown={onKeyDown}>
      {cameras.error ? <Alert kind="error">The camera list could not be loaded: {cameras.error}</Alert> : null}

      <div className="panel">
        <div className="filter-row">
          {picked.map((id, i) => (
            <div key={i} className="tlp-pick">
              <SelectField
                label={i === 0 ? 'Camera' : `Camera ${i + 1}`}
                value={id}
                onChange={(value) => setPicked((current) => current.map((old, j) => (j === i ? value : old)))}
                disabled={cameras.data === null}
              >
                <option value="">{cameras.loading ? 'Loading…' : 'Choose a camera'}</option>
                {cameraList.map((camera) => (
                  <option key={camera.id} value={camera.id} disabled={camera.id !== id && picked.includes(camera.id)}>
                    {camera.deleted ? `${camera.name} (deleted camera)` : camera.name}
                  </option>
                ))}
              </SelectField>
              {i > 0 ? (
                <button
                  type="button"
                  className="button button-small"
                  aria-label={`Remove camera ${i + 1}`}
                  onClick={() => setPicked((current) => current.filter((_, j) => j !== i))}
                >
                  Remove
                </button>
              ) : null}
            </div>
          ))}
          {picked.length < MAX_SYNC_PLAYERS && cameraList.length > picked.length ? (
            <button type="button" className="button" onClick={() => setPicked((current) => [...current, ''])}>
              <Icon name="plus" size={18} />
              Add a camera
            </button>
          ) : null}
          <div className="tlp-date">
            <TextField label="Date" type="date" value={date} onChange={setDate} error={dateError} max={todayLocal()} />
            <div className="button-row">
              <button type="button" className="button button-small" onClick={() => setDate(shiftDate(date, -1))} disabled={!day}>
                <Icon name="back" size={16} />
                Day before
              </button>
              <button type="button" className="button button-small" onClick={() => setDate(shiftDate(date, 1))} disabled={!day || date >= todayLocal()}>
                Day after
                <Icon name="next" size={16} />
              </button>
            </div>
          </div>
        </div>
      </div>

      {failure ? <Alert kind="error">The timeline could not be loaded: {failure}</Alert> : null}
      {otherZone && serverOffset !== null ? (
        <p className="muted tlp-zone">
          Times are shown in this device's time zone. The ButterCams server's clock is at {formatOffset(serverOffset)}.
        </p>
      ) : null}

      {selected.length === 0 ? (
        <p className="muted">Choose a camera to see its day.</p>
      ) : !day ? null : (
        <>
          <div ref={gridRef} className={`tlp-grid tlp-grid-${selected.length}`}>
            {selected.map((id) => (
              <PlayerTile
                key={id}
                cameraId={id}
                name={names.get(id) ?? id}
                fps={fpsOf.get(id)}
                spans={data[id]?.spans ?? []}
                state={states[id]}
                showName={selected.length > 1}
                onPlayer={onPlayer}
                onState={onState}
                onWantSpans={onWantSpans}
              />
            ))}
          </div>

          <div className="tlp-bar" role="group" aria-label="Playback controls">
            <div className="button-row">
              <button type="button" className="button button-icon" onClick={() => step(-1)} disabled={playheadMs === null} aria-label="One frame back" title="One frame back ( , )">
                <Glyph d={GLYPHS.stepBack} />
              </button>
              <button
                type="button"
                className="button button-primary button-icon"
                onClick={togglePlay}
                disabled={!loaded || !anyRecording}
                aria-label={playing ? 'Pause' : 'Play'}
                title={playing ? 'Pause (Space)' : 'Play (Space)'}
              >
                <Glyph d={playing ? GLYPHS.pause : GLYPHS.play} fill={!playing} />
              </button>
              <button type="button" className="button button-icon" onClick={() => step(1)} disabled={playheadMs === null} aria-label="One frame forward" title="One frame forward ( . )">
                <Glyph d={GLYPHS.stepForward} />
              </button>
            </div>
            <p className="tlp-clock mono" aria-live="off">
              {playheadMs === null ? '--:--:--' : clockTime(playheadMs)}
              {playheadMs !== null ? <span>{new Date(playheadMs).toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' })}</span> : null}
            </p>
            <Segmented<string>
              label="Speed"
              value={String(rate)}
              onChange={(value) => changeRate(Number(value))}
              options={SPEEDS.map((s) => ({ value: String(s), label: `${s}×`, ariaLabel: `${s} times speed` }))}
            />
            <div className="button-row">
              <button
                type="button"
                className="button button-icon"
                onClick={() => changeMuted(!muted)}
                aria-pressed={!muted}
                aria-label={muted ? 'Turn sound on' : 'Turn sound off'}
                title={audioAllowed(rate) ? undefined : 'Sound plays at 0.5× to 2×'}
              >
                <Glyph d={muted || !audioAllowed(rate) ? GLYPHS.muted : GLYPHS.sound} />
              </button>
              {currentSpan && !currentSpan.open ? (
                <a className="button" href={api.segmentDownloadUrl(currentSpan.id)} download={currentSpan.fileName}>
                  <Icon name="download" size={18} />
                  Download this hour
                </a>
              ) : (
                <button type="button" className="button" disabled title={currentSpan?.open ? 'Can be downloaded once the file is closed' : undefined}>
                  <Icon name="download" size={18} />
                  Download this hour
                </button>
              )}
              <button type="button" className="button button-icon" onClick={fullScreen} aria-label="Full screen">
                <Icon name="fullscreen" />
              </button>
            </div>
          </div>

          {loaded && !anyRecording ? <p>No recordings were found on this date.</p> : null}

          <Timeline
            day={day}
            rows={rows}
            playheadMs={playheadMs}
            nowMs={now}
            follow={playing}
            onSeek={(ms) => seek(ms, true)}
            onScrub={onScrub}
            onHover={onHover}
            hoverPicture={hoverPicture}
            selection={selection}
            onSelection={setSelection}
          />

          <div className="tlp-foot">
            {selection ? (
              <p className="tlp-selection">
                Selected {clockTime(selection.fromMs)} – {clockTime(selection.toMs)}.{' '}
                <button type="button" className="button button-small" onClick={() => seek(selection.fromMs, true)}>
                  Play from its start
                </button>{' '}
                <button type="button" className="button button-small" onClick={() => setSelection(null)}>
                  Clear
                </button>
              </p>
            ) : (
              <p className="muted">
                Click or drag on the timeline to play from that moment. Scroll or pinch to zoom; hold Shift and drag to select a range.
              </p>
            )}
            {notes.length > 0 ? (
              <details className="tlp-notes">
                <summary>
                  Gaps and recorder events on this day ({notes.length}
                  {notes.length === 300 ? '+' : ''})
                </summary>
                <ul className="plain-list">
                  {notes.map((note, i) => (
                    <li key={i}>
                      <button type="button" className="tlp-note" onClick={() => seek(note.ms, true)}>
                        {rows.length > 1 ? `${note.camera} · ` : ''}
                        {note.text}
                      </button>
                    </li>
                  ))}
                </ul>
              </details>
            ) : null}
          </div>
        </>
      )}
    </section>
  );
}
