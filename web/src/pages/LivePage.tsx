import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Camera, CameraState } from '../api/types';
import { Alert } from '../components/Alert';
import { Icon } from '../components/Icon';
import { PageHeader, Segmented } from '../components/PageHeader';
import { formatCodec, formatResolution } from '../lib/format';
import { REFRESH_MS, useLoad, usePageTitle } from '../lib/hooks';
import type { LiveView, TileLayout } from '../lib/liveViews';
import { ALL_VIEW_ID, LAYOUTS, loadLiveViews, newViewId, saveLiveViews } from '../lib/liveViews';

type Mode = { kind: 'grid' } | { kind: 'single'; cameraId: string } | { kind: 'edit'; viewId: string | null };

type TileState = CameraState | 'unknown';

const CHIP: Record<TileState, string> = {
  recording: 'Recording',
  connecting: 'Connecting',
  backoff: 'No video',
  error: 'Error',
  disabled: 'Disabled',
  unknown: 'No status',
};

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

function Tile({ camera, onOpen }: { camera: Camera; onOpen: () => void }) {
  const state = tileState(camera);
  const problem = problemText(camera, state);
  return (
    <button type="button" className="tile" onClick={onOpen} aria-label={`${camera.name}: ${CHIP[state]}. Open larger.`}>
      <span className="tile-art">
        <Icon name="cameras" size={40} />
      </span>
      <span className="tile-name">{camera.name}</span>
      <span className={`tile-chip tile-chip-${state}`}>{CHIP[state]}</span>
      {problem ? (
        <span className={`tile-problem tile-problem-${state}`}>{problem}</span>
      ) : (
        <span className="tile-meta">{metaText(camera)}</span>
      )}
    </button>
  );
}

export function LivePage() {
  usePageTitle('Live');
  const cameras = useLoad(api.cameras, REFRESH_MS);
  const [stored, setStored] = useState(loadLiveViews);
  const [mode, setMode] = useState<Mode>({ kind: 'grid' });
  const [page, setPage] = useState(0);
  const gridRef = useRef<HTMLDivElement>(null);

  useEffect(() => saveLiveViews(stored), [stored]);

  const cameraList = cameras.data?.cameras ?? [];
  const byId = new Map(cameraList.map((camera) => [camera.id, camera]));

  const allView: LiveView = {
    id: ALL_VIEW_ID,
    name: 'All cameras',
    cameraIds: cameraList.map((camera) => camera.id),
    layout: stored.allLayout,
  };
  const views = [allView, ...stored.views];
  const current = views.find((view) => view.id === stored.selected) ?? allView;
  const viewCameras = current.cameraIds.map((id) => byId.get(id)).filter((camera): camera is Camera => camera !== undefined);

  const pages = current.id === ALL_VIEW_ID ? Math.max(1, Math.ceil(viewCameras.length / current.layout)) : 1;
  const pageIndex = Math.min(page, pages - 1);
  const shown = viewCameras.slice(pageIndex * current.layout, (pageIndex + 1) * current.layout);
  const empty = current.id === ALL_VIEW_ID ? 0 : Math.max(0, current.layout - shown.length);

  function select(viewId: string) {
    setStored((s) => ({ ...s, selected: viewId }));
    setPage(0);
    setMode({ kind: 'grid' });
  }

  function setLayout(layout: TileLayout) {
    setPage(0);
    setStored((s) =>
      current.id === ALL_VIEW_ID
        ? { ...s, allLayout: layout }
        : { ...s, views: s.views.map((view) => (view.id === current.id ? { ...view, layout } : view)) },
    );
  }

  function fullScreen() {
    void gridRef.current?.requestFullscreen?.().catch(() => undefined);
  }

  return (
    <>
      <PageHeader eyebrow={`Live · ${stored.views.length === 1 ? '1 saved view' : `${stored.views.length} saved views`}`} title="Live">
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
            {current.id !== ALL_VIEW_ID ? (
              <button type="button" className="button" onClick={() => setMode({ kind: 'edit', viewId: current.id })}>
                <Icon name="edit" size={18} />
                Edit view
              </button>
            ) : null}
            <button type="button" className="button button-primary button-icon" aria-label="Full screen" onClick={fullScreen}>
              <Icon name="fullscreen" />
            </button>
          </>
        ) : null}
      </PageHeader>

      <div className="view-tabs" role="group" aria-label="Saved views">
        {views.map((view) => (
          <button
            key={view.id}
            type="button"
            className={view.id === current.id && mode.kind !== 'edit' ? 'view-tab view-tab-on' : 'view-tab'}
            aria-pressed={view.id === current.id}
            onClick={() => select(view.id)}
          >
            {view.name || 'Untitled view'}
            <span className="seg-count">{view.id === ALL_VIEW_ID ? cameraList.length : view.cameraIds.length}</span>
          </button>
        ))}
        <button type="button" className="view-tab view-tab-new" onClick={() => setMode({ kind: 'edit', viewId: null })}>
          <Icon name="plus" size={16} />
          New view
        </button>
      </div>

      {cameras.error ? <Alert kind="error">Cameras could not be refreshed: {cameras.error}</Alert> : null}
      {!cameras.data && cameras.loading ? <p className="muted">Loading…</p> : null}

      {cameras.data && mode.kind === 'grid' ? (
        cameraList.length === 0 ? (
          <p>
            No cameras have been added yet. <Link to="/cameras">Add a camera</Link>
          </p>
        ) : (
          <section aria-label={`${current.name} cameras`}>
            <div ref={gridRef} className={`tile-grid tile-grid-${current.layout}`}>
              {shown.map((camera) => (
                <Tile key={camera.id} camera={camera} onOpen={() => setMode({ kind: 'single', cameraId: camera.id })} />
              ))}
              {Array.from({ length: empty }, (_, i) => (
                <button key={`empty-${i}`} type="button" className="tile-empty" onClick={() => setMode({ kind: 'edit', viewId: current.id })}>
                  <Icon name="plus" size={18} />
                  Add a camera
                </button>
              ))}
            </div>
            <div className="grid-note">
              <span>
                {current.id === ALL_VIEW_ID
                  ? `${viewCameras.length === 0 ? 0 : pageIndex * current.layout + 1}–${pageIndex * current.layout + shown.length} of ${viewCameras.length} cameras`
                  : `${viewCameras.length} ${viewCameras.length === 1 ? 'camera' : 'cameras'} in this view`}
              </span>
              {pages > 1 ? (
                <span className="button-row">
                  <button type="button" className="button button-small" disabled={pageIndex === 0} onClick={() => setPage(pageIndex - 1)}>
                    <Icon name="back" size={16} />
                    Previous
                  </button>
                  <button type="button" className="button button-small" disabled={pageIndex >= pages - 1} onClick={() => setPage(pageIndex + 1)}>
                    Next
                    <Icon name="next" size={16} />
                  </button>
                </span>
              ) : (
                <span>Click a tile to open it larger. States refresh every 5 seconds.</span>
              )}
            </div>
          </section>
        )
      ) : null}

      {cameras.data && mode.kind === 'single' ? (
        <SingleCamera
          camera={byId.get(mode.cameraId) ?? null}
          others={viewCameras.filter((camera) => camera.id !== mode.cameraId).slice(0, 8)}
          viewName={current.name}
          onBack={() => setMode({ kind: 'grid' })}
          onOpen={(cameraId) => setMode({ kind: 'single', cameraId })}
        />
      ) : null}

      {cameras.data && mode.kind === 'edit' ? (
        <ViewEditor
          key={mode.viewId ?? 'new'}
          view={mode.viewId === null ? null : (stored.views.find((view) => view.id === mode.viewId) ?? null)}
          cameras={cameraList}
          onCancel={() => setMode({ kind: 'grid' })}
          onSave={(view) => {
            setStored((s) => {
              const exists = s.views.some((existing) => existing.id === view.id);
              return {
                ...s,
                selected: view.id,
                views: exists ? s.views.map((existing) => (existing.id === view.id ? view : existing)) : [...s.views, view],
              };
            });
            setPage(0);
            setMode({ kind: 'grid' });
          }}
          onDelete={(viewId) => {
            setStored((s) => ({ ...s, selected: ALL_VIEW_ID, views: s.views.filter((view) => view.id !== viewId) }));
            setMode({ kind: 'grid' });
          }}
        />
      ) : null}
    </>
  );
}

function SingleCamera({
  camera,
  others,
  viewName,
  onBack,
  onOpen,
}: {
  camera: Camera | null;
  others: Camera[];
  viewName: string;
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
  const state = tileState(camera);
  const problem = problemText(camera, state);
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
          <span className="eyebrow">{metaText(camera) || camera.host}</span>
        </div>
        <div className="single-frame">
          <span className={`tile-chip tile-chip-${state}`} style={{ top: 14, right: 14 }}>
            {CHIP[state]}
          </span>
          <div className="single-message">
            <Icon name="cameras" size={64} />
            <p>{problem ?? 'Recording now. Live video is not part of this version of ButterCams yet; open the recordings to watch.'}</p>
          </div>
          <div className="single-bar">
            <Link to="/recordings" className="button">
              <Icon name="clock" size={18} />
              Recordings
            </Link>
            <Link to="/cameras" className="button">
              <Icon name="edit" size={18} />
              Camera settings
            </Link>
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
  cameras,
  onCancel,
  onSave,
  onDelete,
}: {
  view: LiveView | null;
  cameras: Camera[];
  onCancel: () => void;
  onSave: (view: LiveView) => void;
  onDelete: (viewId: string) => void;
}) {
  const [name, setName] = useState(view?.name ?? '');
  const [layout, setLayout] = useState<TileLayout>(view?.layout ?? 4);
  const [picked, setPicked] = useState<string[]>(view?.cameraIds ?? []);
  const [nameError, setNameError] = useState<string | null>(null);

  function toggle(cameraId: string) {
    setPicked((current) => (current.includes(cameraId) ? current.filter((id) => id !== cameraId) : [...current, cameraId]));
  }

  function save() {
    const trimmed = name.trim();
    if (trimmed === '') {
      setNameError('Give the view a name.');
      return;
    }
    onSave({ id: view?.id ?? newViewId(), name: trimmed, layout, cameraIds: picked });
  }

  const over = picked.length > layout;

  return (
    <section className="panel panel-strong" aria-labelledby="editor-heading">
      <div className="panel-header">
        <h2 id="editor-heading">{view ? 'Edit view' : 'New view'}</h2>
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
                For example Entrances or Warehouse. Views are saved in this browser.
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
          <button type="button" className="button button-danger" onClick={() => onDelete(view.id)}>
            Delete view
          </button>
        ) : (
          <span />
        )}
        <div className="button-row">
          <button type="button" className="button" onClick={onCancel}>
            Cancel
          </button>
          <button type="button" className="button button-primary" onClick={save}>
            Save view
          </button>
        </div>
      </div>
    </section>
  );
}
