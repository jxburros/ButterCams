import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { RefObject, TouchEvent } from 'react';
import { Link } from 'react-router-dom';
import { api, errorMessage } from '../api/client';
import type { Camera, CameraState, LiveViewsResponse, StreamKind } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { Alert } from '../components/Alert';
import { Icon } from '../components/Icon';
import { PageHeader, Segmented } from '../components/PageHeader';
import { formatCodec, formatResolution } from '../lib/format';
import { REFRESH_MS, useLoad, usePageTitle } from '../lib/hooks';
import type { LiveView, TileLayout, ViewScope } from '../lib/liveViews';
import {
  ALL_VIEW_ID,
  LAYOUTS,
  MAX_VIEWS,
  legacyHandled,
  loadPrefs,
  mergeImported,
  newViewId,
  pendingLegacyViews,
  savePrefs,
  sharedCopy,
  upsertView,
  viewKey,
} from '../lib/liveViews';
import { LivePlayer } from '../player/LivePlayer';
import type { LiveStatus } from '../player/LivePlayer';
import { pickStream, unsupportedMessage } from '../player/liveLogic';
import { NO_MSE_MESSAGE, mseAvailable } from '../player/mse';
import '../live.css';

type Mode =
  | { kind: 'grid' }
  | { kind: 'single'; cameraId: string }
  | { kind: 'edit'; scope: 'mine' | 'shared'; viewId: string | null };

type TileState = CameraState | 'unknown';

const CHIP: Record<TileState, string> = {
  recording: 'Recording',
  connecting: 'Connecting',
  backoff: 'No video',
  error: 'Error',
  disabled: 'Disabled',
  unknown: 'No status',
};

/** Phones: at most four tiles per page (spec 16.4). */
const NARROW = '(max-width: 699px)';
const PHONE_TILES = 4;

function tileState(camera: Camera): TileState {
  if (!camera.enabled) return 'disabled';
  return camera.stats?.state ?? 'unknown';
}

function problemText(camera: Camera, state: TileState): string | null {
  if (state === 'recording') return null;
  if (state === 'disabled') return 'Recording is turned off for this camera';
  if (state === 'unknown') return 'Waiting for the recorder to report';
  if (camera.stats && camera.stats.message !== '') return camera.stats.message;
  return state === 'connecting' ? 'Connecting to the camera…' : 'Not recording';
}

function metaText(camera: Camera): string {
  const stats = camera.stats;
  if (!stats) return '';
  return `${formatCodec(stats.videoCodec)} · ${formatResolution(stats.width, stats.height)}`;
}

// ------------------------------------------------------------------ hooks

function useMedia(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const list = window.matchMedia(query);
    const update = () => setMatches(list.matches);
    update();
    list.addEventListener('change', update);
    return () => list.removeEventListener('change', update);
  }, [query]);
  return matches;
}

/** `true` while the element is on screen and the tab is in front. */
function useVisible(ref: RefObject<Element | null>): boolean {
  const [onScreen, setOnScreen] = useState(false);
  const [pageShown, setPageShown] = useState(() => document.visibilityState === 'visible');
  useEffect(() => {
    const update = () => setPageShown(document.visibilityState === 'visible');
    document.addEventListener('visibilitychange', update);
    return () => document.removeEventListener('visibilitychange', update);
  }, []);
  useEffect(() => {
    const element = ref.current;
    if (element === null) return;
    if (typeof IntersectionObserver === 'undefined') {
      setOnScreen(true);
      return;
    }
    const observer = new IntersectionObserver((entries) => {
      const last = entries[entries.length - 1];
      if (last) setOnScreen(last.isIntersecting);
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, [ref]);
  return onScreen && pageShown;
}

interface LiveStream {
  status: LiveStatus;
  /** The stream in use, or `null` when none of the camera's streams can be played here. */
  stream: StreamKind | null;
  /** What to tell the user while there is no picture; `null` while playing or still connecting. */
  message: string | null;
  playing: boolean;
}

/**
 * Plays a camera into the video element while `active`. A grid tile uses the
 * sub stream, single view the main stream; when the browser cannot play one
 * (H.265), the other is tried (spec 16.4).
 */
function useLiveStream(
  videoRef: RefObject<HTMLVideoElement | null>,
  camera: Camera,
  mode: 'tile' | 'single',
  mainEnabled: boolean,
  active: boolean,
): LiveStream {
  const [unsupported, setUnsupported] = useState<{ streams: Set<StreamKind>; mime: string }>({
    streams: new Set(),
    mime: '',
  });
  const [status, setStatus] = useState<LiveStatus>({ kind: 'idle' });
  const hasSub = camera.subStreamPath !== null && camera.subStreamPath !== '';
  const stream = pickStream({ mode, hasSub, mainEnabled, unsupported: unsupported.streams });

  useEffect(() => {
    const video = videoRef.current;
    if (!active || stream === null || video === null) {
      setStatus({ kind: 'idle' });
      return;
    }
    const player = new LivePlayer(video, {
      camera: camera.id,
      stream,
      audio: mode === 'single',
      onStatus: (next) => {
        if (next.kind === 'unsupported') {
          setUnsupported((prev) => ({ streams: new Set(prev.streams).add(stream), mime: next.mime }));
        }
        setStatus(next);
      },
    });
    player.start();
    return () => player.stop();
  }, [videoRef, camera.id, mode, stream, active]);

  let message: string | null = null;
  if (!mseAvailable()) message = NO_MSE_MESSAGE;
  else if (stream === null) {
    message =
      unsupported.streams.size > 0
        ? unsupportedMessage(unsupported.mime)
        : 'This camera has no sub stream and the main stream is switched off for live view.';
  } else if (status.kind === 'waiting') message = status.message || 'Waiting for video…';
  else if (status.kind === 'unavailable') message = status.message;
  else if (status.kind === 'error') message = status.message;

  return { status, stream, message, playing: status.kind === 'playing' };
}

// ------------------------------------------------------------------- tile

function Tile({
  camera,
  live,
  mainEnabled,
  onOpen,
}: {
  camera: Camera;
  /** `false` when live view is switched off: the tile then shows states only. */
  live: boolean;
  mainEnabled: boolean;
  onOpen: () => void;
}) {
  const ref = useRef<HTMLButtonElement>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const visible = useVisible(ref);
  const stream = useLiveStream(videoRef, camera, 'tile', mainEnabled, live && visible);
  const state = tileState(camera);
  const problem = problemText(camera, state) ?? (stream.playing ? null : stream.message);
  return (
    <button
      ref={ref}
      type="button"
      className="tile"
      onClick={onOpen}
      aria-label={`${camera.name}: ${CHIP[state]}${stream.playing ? ', live' : ''}. Open larger.`}
    >
      <span className="tile-art">
        <Icon name="cameras" size={40} />
      </span>
      <video
        ref={videoRef}
        className={stream.playing ? 'tile-video tile-video-on' : 'tile-video'}
        muted
        playsInline
        autoPlay
        disablePictureInPicture
        aria-hidden="true"
        tabIndex={-1}
      />
      <span className="tile-name">{camera.name}</span>
      <span className={`tile-chip tile-chip-${state}`}>{CHIP[state]}</span>
      {problem ? (
        <span className={`tile-problem tile-problem-${state === 'recording' ? 'connecting' : state}`}>{problem}</span>
      ) : stream.playing ? null : (
        <span className="tile-meta">{metaText(camera)}</span>
      )}
    </button>
  );
}

// ------------------------------------------------------------------- page

interface PickerView extends LiveView {
  scope: ViewScope;
  key: string;
}

export function LivePage() {
  usePageTitle('Live');
  const { isAdmin } = useAuth();
  const cameras = useLoad(api.cameras, REFRESH_MS);
  const system = useLoad(api.system, 30000);
  const saved = useLoad(api.liveViews);
  const [prefs, setPrefs] = useState(loadPrefs);
  const [mode, setMode] = useState<Mode>({ kind: 'grid' });
  const [page, setPage] = useState(0);
  const [layoutOverride, setLayoutOverride] = useState<Record<string, TileLayout>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [legacy, setLegacy] = useState(pendingLegacyViews);
  const gridRef = useRef<HTMLDivElement>(null);
  const touchStart = useRef<number | null>(null);
  const narrow = useMedia(NARROW);

  useEffect(() => savePrefs(prefs), [prefs]);

  const cameraList = cameras.data?.cameras ?? [];
  const byId = new Map(cameraList.map((camera) => [camera.id, camera]));
  const mine = saved.data?.mine ?? [];
  const shared = saved.data?.shared ?? [];
  const liveOn = system.data ? system.data.live.enabled : true;
  const mainEnabled = system.data ? system.data.live.mainStream : true;

  const views: PickerView[] = useMemo(() => {
    const withLayout = (view: LiveView, scope: ViewScope): PickerView => {
      const key = viewKey(scope, view.id);
      return { ...view, scope, key, layout: layoutOverride[key] ?? view.layout };
    };
    return [
      ...(saved.data?.mine ?? []).map((view) => withLayout(view, 'mine')),
      ...(saved.data?.shared ?? []).map((view) => withLayout(view, 'shared')),
    ];
  }, [saved.data, layoutOverride]);

  const allView: PickerView = {
    id: ALL_VIEW_ID,
    key: ALL_VIEW_ID,
    scope: 'all',
    name: 'All cameras',
    cameraIds: cameraList.map((camera) => camera.id),
    layout: prefs.allLayout,
  };
  const current = views.find((view) => view.key === prefs.selected) ?? allView;
  const canEdit = current.scope === 'mine' || (current.scope === 'shared' && isAdmin);
  const viewCameras = current.cameraIds.map((id) => byId.get(id)).filter((camera): camera is Camera => camera !== undefined);

  const perPage = narrow ? (Math.min(current.layout, PHONE_TILES) as TileLayout) : current.layout;
  const pages = Math.max(1, Math.ceil(viewCameras.length / perPage));
  const pageIndex = Math.min(page, pages - 1);
  const shown = viewCameras.slice(pageIndex * perPage, (pageIndex + 1) * perPage);
  const empty = canEdit && pages === 1 && !narrow ? Math.max(0, perPage - shown.length) : 0;

  function select(key: string) {
    setPrefs((p) => ({ ...p, selected: key }));
    setPage(0);
    setMode({ kind: 'grid' });
    setNotice(null);
  }

  /** Saves one of the two lists on the server and shows the server's answer. */
  const store = useCallback(
    async (scope: 'mine' | 'shared', list: LiveView[]): Promise<LiveViewsResponse | null> => {
      setSaving(true);
      setSaveError(null);
      try {
        const response = scope === 'mine' ? await api.saveMyViews(list) : await api.saveSharedViews(list);
        saved.replace(response);
        return response;
      } catch (err) {
        setSaveError(errorMessage(err));
        return null;
      } finally {
        setSaving(false);
      }
    },
    [saved],
  );

  function setLayout(layout: TileLayout) {
    setPage(0);
    if (current.scope === 'all') {
      setPrefs((p) => ({ ...p, allLayout: layout }));
      return;
    }
    // Shown at once; kept on the server when this user may edit the view,
    // otherwise it lasts until the page is left.
    setLayoutOverride((o) => ({ ...o, [current.key]: layout }));
    if (canEdit) {
      const scope = current.scope;
      const list = (scope === 'mine' ? mine : shared).map((view) => (view.id === current.id ? { ...view, layout } : view));
      void store(scope, list);
    }
  }

  async function saveView(scope: 'mine' | 'shared', view: LiveView) {
    const list = scope === 'mine' ? mine : shared;
    if (!list.some((existing) => existing.id === view.id) && list.length >= MAX_VIEWS) {
      setSaveError(`At most ${MAX_VIEWS} views can be saved. Delete one first.`);
      return;
    }
    const key = viewKey(scope, view.id);
    if ((await store(scope, upsertView(list, view))) === null) return;
    setLayoutOverride(({ [key]: _dropped, ...rest }) => rest);
    select(key);
  }

  async function deleteView(scope: 'mine' | 'shared', viewId: string) {
    const list = scope === 'mine' ? mine : shared;
    if ((await store(scope, list.filter((view) => view.id !== viewId))) === null) return;
    select(ALL_VIEW_ID);
  }

  async function shareView(view: LiveView) {
    if (shared.length >= MAX_VIEWS) {
      setSaveError(`At most ${MAX_VIEWS} shared views can be saved. Delete one first.`);
      return;
    }
    const copy = sharedCopy(view, shared);
    if ((await store('shared', [...shared, copy])) === null) return;
    select(viewKey('shared', copy.id));
    setNotice(`“${copy.name}” is now a shared view. Every user sees it, with the cameras they may see.`);
  }

  async function importLegacy() {
    const merged = mergeImported(mine, legacy);
    if ((await store('mine', merged)) === null) return;
    legacyHandled();
    setLegacy([]);
    setNotice('The views from this browser were added to your views.');
  }

  function fullScreen() {
    void gridRef.current?.requestFullscreen?.().catch(() => undefined);
  }

  function onTouchStart(event: TouchEvent) {
    touchStart.current = event.touches.length === 1 ? (event.touches[0]?.clientX ?? null) : null;
  }

  function onTouchEnd(event: TouchEvent) {
    const from = touchStart.current;
    touchStart.current = null;
    const to = event.changedTouches[0]?.clientX;
    if (from === null || to === undefined || pages < 2) return;
    if (to - from < -50 && pageIndex < pages - 1) setPage(pageIndex + 1);
    if (to - from > 50 && pageIndex > 0) setPage(pageIndex - 1);
  }

  const tab = (view: PickerView) => (
    <button
      key={view.key}
      type="button"
      className={view.key === current.key && mode.kind !== 'edit' ? 'view-tab view-tab-on' : 'view-tab'}
      aria-pressed={view.key === current.key}
      onClick={() => select(view.key)}
    >
      {view.name || 'Untitled view'}
      <span className="seg-count">{view.cameraIds.filter((id) => byId.has(id)).length}</span>
    </button>
  );
  const myTabs = views.filter((view) => view.scope === 'mine');
  const sharedTabs = views.filter((view) => view.scope === 'shared');
  const editing =
    mode.kind === 'edit' && mode.viewId !== null
      ? ((mode.scope === 'mine' ? mine : shared).find((view) => view.id === mode.viewId) ?? null)
      : null;

  return (
    <>
      <PageHeader
        eyebrow={`Live · ${mine.length === 1 ? '1 view of yours' : `${mine.length} views of yours`} · ${shared.length} shared`}
        title="Live"
      >
        {mode.kind === 'grid' ? (
          <>
            <Segmented<string>
              label="Layout"
              value={String(current.layout)}
              onChange={(value) => setLayout(Number(value) as TileLayout)}
              options={LAYOUTS.map((n) => ({
                value: String(n),
                ariaLabel: n === 1 ? '1 tile' : `${n} tiles`,
                label: (
                  <>
                    <Icon name="grid" size={16} />
                    {n}
                  </>
                ),
              }))}
            />
            {canEdit && current.scope !== 'all' ? (
              <button
                type="button"
                className="button"
                onClick={() => setMode({ kind: 'edit', scope: current.scope === 'shared' ? 'shared' : 'mine', viewId: current.id })}
              >
                <Icon name="edit" size={18} />
                Edit view
              </button>
            ) : null}
            {isAdmin && current.scope === 'mine' ? (
              <button type="button" className="button" disabled={saving} onClick={() => void shareView(current)}>
                Share with everyone
              </button>
            ) : null}
            <button type="button" className="button button-primary button-icon" aria-label="Full screen" onClick={fullScreen}>
              <Icon name="fullscreen" />
            </button>
          </>
        ) : null}
      </PageHeader>

      <div className="view-tabs" role="group" aria-label="Views">
        {tab(allView)}
        {myTabs.length > 0 ? <span className="view-group">My views</span> : null}
        {myTabs.map(tab)}
        {sharedTabs.length > 0 ? <span className="view-group">Shared views</span> : null}
        {sharedTabs.map(tab)}
        <button type="button" className="view-tab view-tab-new" onClick={() => setMode({ kind: 'edit', scope: 'mine', viewId: null })}>
          <Icon name="plus" size={16} />
          New view
        </button>
      </div>

      {legacy.length > 0 && saved.data ? (
        <div className="alert alert-info live-import" role="status">
          <span>
            This browser holds {legacy.length === 1 ? '1 view' : `${legacy.length} views`} saved by an earlier version of ButterCams (
            {legacy.map((view) => view.name).join(', ')}). Views are now kept with your account. Add {legacy.length === 1 ? 'it' : 'them'} to
            your views?
          </span>
          <span className="button-row">
            <button type="button" className="button button-small button-primary" disabled={saving} onClick={() => void importLegacy()}>
              Add to my views
            </button>
            <button
              type="button"
              className="button button-small"
              onClick={() => {
                legacyHandled();
                setLegacy([]);
              }}
            >
              No, thanks
            </button>
          </span>
        </div>
      ) : null}

      {notice ? <Alert kind="success">{notice}</Alert> : null}
      {saveError ? <Alert kind="error">The view could not be saved: {saveError}</Alert> : null}
      {saved.error ? <Alert kind="error">Saved views could not be loaded: {saved.error}</Alert> : null}
      {cameras.error ? <Alert kind="error">Cameras could not be refreshed: {cameras.error}</Alert> : null}
      {!liveOn ? (
        <Alert kind="info">
          Live view is switched off{isAdmin ? <> on the <Link to="/system">System page</Link></> : ' by an administrator'}. The tiles show
          each camera’s recording state.
        </Alert>
      ) : null}
      {!cameras.data && cameras.loading ? <p className="muted">Loading…</p> : null}

      {cameras.data && mode.kind === 'grid' ? (
        cameraList.length === 0 ? (
          <p>
            No cameras {isAdmin ? 'have been added yet.' : 'are available to your account.'}{' '}
            {isAdmin ? <Link to="/cameras">Add a camera</Link> : null}
          </p>
        ) : (
          <section aria-label={`${current.name} cameras`}>
            <div
              ref={gridRef}
              className={`tile-grid tile-grid-${perPage} live-grid`}
              onTouchStart={onTouchStart}
              onTouchEnd={onTouchEnd}
            >
              {shown.map((camera) => (
                <Tile
                  key={camera.id}
                  camera={camera}
                  live={liveOn}
                  mainEnabled={mainEnabled}
                  onOpen={() => setMode({ kind: 'single', cameraId: camera.id })}
                />
              ))}
              {Array.from({ length: empty }, (_, i) => (
                <button
                  key={`empty-${i}`}
                  type="button"
                  className="tile-empty"
                  onClick={() => setMode({ kind: 'edit', scope: current.scope === 'shared' ? 'shared' : 'mine', viewId: current.id })}
                >
                  <Icon name="plus" size={18} />
                  Add a camera
                </button>
              ))}
            </div>
            <div className="grid-note">
              <span>
                {viewCameras.length === 0
                  ? 'No cameras in this view'
                  : `${pageIndex * perPage + 1}–${pageIndex * perPage + shown.length} of ${viewCameras.length} ${viewCameras.length === 1 ? 'camera' : 'cameras'}`}
              </span>
              {pages > 1 ? (
                <span className="button-row">
                  <button
                    type="button"
                    className="button button-small"
                    disabled={pageIndex === 0}
                    onClick={() => setPage(pageIndex - 1)}
                    aria-label="Previous page of cameras"
                  >
                    <Icon name="back" size={16} />
                    Previous
                  </button>
                  <span className="live-page-count" aria-live="polite">
                    {pageIndex + 1} / {pages}
                  </span>
                  <button
                    type="button"
                    className="button button-small"
                    disabled={pageIndex >= pages - 1}
                    onClick={() => setPage(pageIndex + 1)}
                    aria-label="Next page of cameras"
                  >
                    Next
                    <Icon name="next" size={16} />
                  </button>
                </span>
              ) : (
                <span>Click a tile for the full picture and sound.</span>
              )}
            </div>
          </section>
        )
      ) : null}

      {cameras.data && mode.kind === 'single' ? (
        <SingleCamera
          key={mode.cameraId}
          camera={byId.get(mode.cameraId) ?? null}
          others={viewCameras.filter((camera) => camera.id !== mode.cameraId).slice(0, 8)}
          viewName={current.name}
          live={liveOn}
          mainEnabled={mainEnabled}
          isAdmin={isAdmin}
          onBack={() => setMode({ kind: 'grid' })}
          onOpen={(cameraId) => setMode({ kind: 'single', cameraId })}
        />
      ) : null}

      {cameras.data && mode.kind === 'edit' ? (
        <ViewEditor
          key={`${mode.scope}:${mode.viewId ?? 'new'}`}
          view={editing}
          scope={mode.scope}
          cameras={cameraList}
          busy={saving}
          canShare={isAdmin && mode.scope === 'mine'}
          onCancel={() => setMode({ kind: 'grid' })}
          onSave={(view) => void saveView(mode.scope, view)}
          onDelete={(viewId) => void deleteView(mode.scope, viewId)}
          onShare={(view) =>
            void (async () => {
              // Keep the edits in the user's own view too, then share that.
              if ((await store('mine', upsertView(mine, view))) !== null) await shareView(view);
            })()
          }
        />
      ) : null}
    </>
  );
}

function SingleCamera({
  camera,
  others,
  viewName,
  live,
  mainEnabled,
  isAdmin,
  onBack,
  onOpen,
}: {
  camera: Camera | null;
  others: Camera[];
  viewName: string;
  live: boolean;
  mainEnabled: boolean;
  isAdmin: boolean;
  onBack: () => void;
  onOpen: (cameraId: string) => void;
}) {
  if (camera === null) {
    return (
      <div className="panel">
        <p>This camera no longer exists.</p>
        <button type="button" className="button" onClick={onBack}>
          Back to {viewName}
        </button>
      </div>
    );
  }
  return (
    <SingleView
      camera={camera}
      others={others}
      viewName={viewName}
      live={live}
      mainEnabled={mainEnabled}
      isAdmin={isAdmin}
      onBack={onBack}
      onOpen={onOpen}
    />
  );
}

function SingleView({
  camera,
  others,
  viewName,
  live,
  mainEnabled,
  isAdmin,
  onBack,
  onOpen,
}: {
  camera: Camera;
  others: Camera[];
  viewName: string;
  live: boolean;
  mainEnabled: boolean;
  isAdmin: boolean;
  onBack: () => void;
  onOpen: (cameraId: string) => void;
}) {
  const frameRef = useRef<HTMLDivElement>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const visible = useVisible(frameRef);
  const stream = useLiveStream(videoRef, camera, 'single', mainEnabled, live && visible);
  const [muted, setMuted] = useState(true);
  const state = tileState(camera);
  const problem = problemText(camera, state);
  const hasAudio = (camera.stats?.audioCodec ?? '') !== '';

  useEffect(() => {
    if (videoRef.current) videoRef.current.muted = muted;
  }, [muted, stream.playing]);

  function fullScreen() {
    const frame = frameRef.current;
    const video = videoRef.current as (HTMLVideoElement & { webkitEnterFullscreen?: () => void }) | null;
    if (document.fullscreenElement) {
      void document.exitFullscreen().catch(() => undefined);
    } else if (frame?.requestFullscreen) {
      void frame.requestFullscreen().catch(() => undefined);
    } else {
      video?.webkitEnterFullscreen?.(); // iPhone: only the video itself can go full screen
    }
  }

  const text = stream.playing
    ? null
    : (stream.message ??
      problem ??
      (live ? 'Connecting to live video…' : 'Recording now. Live view is switched off; open the recordings to watch.'));

  return (
    <section className="single" aria-labelledby="single-heading">
      <div className="single-main">
        <div className="single-head">
          <div className="single-head-title">
            <button type="button" className="button" onClick={onBack}>
              <Icon name="back" size={18} />
              {viewName}
            </button>
            <h2 id="single-heading">{camera.name}</h2>
          </div>
          <span className="eyebrow">
            {metaText(camera) || camera.host}
            {stream.playing && stream.stream ? ` · ${stream.stream === 'main' ? 'main stream' : 'sub stream'}` : ''}
          </span>
        </div>
        <div ref={frameRef} className="single-frame">
          <video
            ref={videoRef}
            className={stream.playing ? 'single-video single-video-on' : 'single-video'}
            muted
            playsInline
            autoPlay
            aria-label={`Live video of ${camera.name}`}
          />
          <span className={`tile-chip tile-chip-${state}`} style={{ top: 14, right: 14 }}>
            {CHIP[state]}
          </span>
          {text ? (
            <div className="single-message">
              <Icon name="cameras" size={64} />
              <p>{text}</p>
            </div>
          ) : null}
          <div className="single-bar">
            <span className="button-row">
              {stream.playing && hasAudio ? (
                <button type="button" className="button" aria-pressed={!muted} onClick={() => setMuted((m) => !m)}>
                  {muted ? 'Turn sound on' : 'Mute'}
                </button>
              ) : null}
              <button type="button" className="button" onClick={fullScreen}>
                <Icon name="fullscreen" size={18} />
                Full screen
              </button>
            </span>
            <span className="button-row">
              <Link to="/recordings" className="button">
                <Icon name="clock" size={18} />
                Recordings
              </Link>
              {isAdmin ? (
                <Link to="/cameras" className="button">
                  <Icon name="edit" size={18} />
                  Camera settings
                </Link>
              ) : null}
            </span>
          </div>
        </div>
      </div>
      {others.length > 0 ? (
        <aside className="single-side" aria-labelledby="others-heading">
          <p id="others-heading" className="side-title">
            Also in {viewName}
          </p>
          {others.map((other) => {
            const otherState = tileState(other);
            return (
              <button key={other.id} type="button" className="side-cam" onClick={() => onOpen(other.id)}>
                <span className="side-thumb">
                  <span className={`tile-chip tile-chip-${otherState}`} aria-hidden="true" />
                </span>
                <span className="side-text">
                  <span>{other.name}</span>
                  <span>{CHIP[otherState]}</span>
                </span>
              </button>
            );
          })}
        </aside>
      ) : null}
    </section>
  );
}

function ViewEditor({
  view,
  scope,
  cameras,
  busy,
  canShare,
  onCancel,
  onSave,
  onDelete,
  onShare,
}: {
  view: LiveView | null;
  scope: 'mine' | 'shared';
  cameras: Camera[];
  busy: boolean;
  /** An Admin editing one of their own views can copy it to the shared list. */
  canShare: boolean;
  onCancel: () => void;
  onSave: (view: LiveView) => void;
  onDelete: (viewId: string) => void;
  onShare: (view: LiveView) => void;
}) {
  const [name, setName] = useState(view?.name ?? '');
  const [layout, setLayout] = useState<TileLayout>(view?.layout ?? 4);
  const [picked, setPicked] = useState<string[]>(view?.cameraIds ?? []);
  const [nameError, setNameError] = useState<string | null>(null);

  function toggle(cameraId: string) {
    setPicked((current) => (current.includes(cameraId) ? current.filter((id) => id !== cameraId) : [...current, cameraId]));
  }

  function current(): LiveView | null {
    const trimmed = name.trim();
    if (trimmed === '') {
      setNameError('Give the view a name.');
      return null;
    }
    return { id: view?.id ?? newViewId(), name: trimmed, layout, cameraIds: picked };
  }

  function save() {
    const value = current();
    if (value) onSave(value);
  }

  function share() {
    const value = current();
    if (value) onShare(value);
  }

  const over = picked.length > layout;

  return (
    <section className="panel panel-strong" aria-labelledby="editor-heading">
      <div className="panel-header">
        <h2 id="editor-heading">
          {view ? (scope === 'shared' ? 'Edit shared view' : 'Edit view') : 'New view'}
        </h2>
        <span className="eyebrow">
          {picked.length} {picked.length === 1 ? 'camera' : 'cameras'} ·{' '}
          {over ? `only the first ${layout} fit this layout` : `${layout - picked.length} empty tiles`}
        </span>
      </div>
      <div className="view-editor">
        <div className="view-editor-settings">
          <div className={nameError ? 'field field-invalid' : 'field'}>
            <label htmlFor="view-name">Name</label>
            <input
              id="view-name"
              type="text"
              value={name}
              maxLength={60}
              aria-invalid={nameError ? true : undefined}
              aria-describedby={nameError ? 'view-name-error' : 'view-name-hint'}
              onChange={(event) => {
                setName(event.target.value);
                setNameError(null);
              }}
            />
            {nameError ? (
              <p id="view-name-error" className="field-error">
                {nameError}
              </p>
            ) : (
              <p id="view-name-hint" className="field-hint">
                {scope === 'shared'
                  ? 'Every user sees this view, with the cameras they are allowed to see.'
                  : 'For example Entrances or Warehouse. Only you see this view, on any device you sign in from.'}
              </p>
            )}
          </div>
          <div className="field">
            <span className="seg-field-label" id="layout-label">
              Layout
            </span>
            <div style={{ marginTop: '0.4rem' }}>
              <Segmented<string>
                label="Layout"
                value={String(layout)}
                onChange={(value) => setLayout(Number(value) as TileLayout)}
                options={LAYOUTS.map((n) => ({ value: String(n), label: String(n), ariaLabel: n === 1 ? '1 tile' : `${n} tiles` }))}
              />
            </div>
          </div>
        </div>
        <fieldset className="view-editor-cameras">
          <legend style={{ padding: 0 }}>Cameras, in tile order</legend>
          {cameras.length === 0 ? (
            <p className="muted">No cameras have been added yet.</p>
          ) : (
            <div className="pick-grid">
              {cameras.map((camera) => {
                const position = picked.indexOf(camera.id);
                const on = position >= 0;
                return (
                  <label key={camera.id} className={on ? 'pick pick-on' : 'pick'}>
                    <input type="checkbox" checked={on} onChange={() => toggle(camera.id)} />
                    <span className="pick-name">{camera.name}</span>
                    <span className="pick-pos" aria-hidden="true">
                      {on ? position + 1 : ''}
                    </span>
                  </label>
                );
              })}
            </div>
          )}
        </fieldset>
      </div>
      <div className="editor-foot">
        {view ? (
          <button type="button" className="button button-danger" disabled={busy} onClick={() => onDelete(view.id)}>
            Delete view
          </button>
        ) : (
          <span />
        )}
        <div className="button-row">
          <button type="button" className="button" disabled={busy} onClick={onCancel}>
            Cancel
          </button>
          {canShare ? (
            <button type="button" className="button" disabled={busy} onClick={share}>
              Share with everyone
            </button>
          ) : null}
          <button type="button" className="button button-primary" disabled={busy} onClick={save}>
            {busy ? 'Saving…' : 'Save view'}
          </button>
        </div>
      </div>
    </section>
  );
}
