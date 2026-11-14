import { useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Camera, CameraStats, Destination, DestinationsResponse, Health } from '../api/types';
import { Alert } from '../components/Alert';
import { CameraStateBadge, DestinationStateBadge } from '../components/Badge';
import { Icon } from '../components/Icon';
import { PageHeader, Segmented } from '../components/PageHeader';
import {
  EMPTY,
  formatAge,
  formatBitrate,
  formatBytes,
  formatCodec,
  formatDuration,
  formatGB,
  formatResolution,
  formatTime,
} from '../lib/format';
import { REFRESH_MS, useLoad, useNow, usePageTitle } from '../lib/hooks';

function audioText(camera: Camera): string {
  const stats = camera.stats;
  if (!camera.recordAudio) return 'Off';
  if (!stats) return EMPTY;
  if (stats.audioNote !== '') return stats.audioCodec ? `${formatCodec(stats.audioCodec)} (${stats.audioNote})` : stats.audioNote;
  return stats.audioCodec ? formatCodec(stats.audioCodec) : 'None';
}

/**
 * Age of the camera's last video packet. Both figures come from the server's
 * clock, so a viewer PC with a wrong clock still sees the right age.
 */
function lastPacketText(stats: CameraStats): string {
  if (stats.reportAgeSec > 20) return `no report for ${formatDuration(stats.reportAgeSec)}`;
  if (stats.lastPacketAgeSec === null) return EMPTY;
  return stats.lastPacketAgeSec < 1 ? 'just now' : `${formatDuration(stats.lastPacketAgeSec)} ago`;
}

type CameraFilter = 'all' | 'attention' | 'recording';

/** An enabled camera that is not recording. */
function needsAttention(camera: Camera): boolean {
  return camera.enabled && camera.stats?.state !== 'recording';
}

function CameraRow({ camera }: { camera: Camera }) {
  const stats = camera.stats;
  return (
    <tr>
      <th scope="row">
        {camera.name}
        <div className="cell-note mono">{camera.host}</div>
      </th>
      <td>
        <CameraStateBadge state={stats?.state ?? (camera.enabled ? null : 'disabled')} />
        {stats && stats.message !== '' ? <div className="cell-note">{stats.message}</div> : null}
      </td>
      <td className="mono-cell nowrap">
        {stats && stats.videoCodec !== '' ? `${formatCodec(stats.videoCodec)} ${formatResolution(stats.width, stats.height)}` : EMPTY}
      </td>
      <td className="num">{stats ? formatBitrate(stats.bitrateKbps) : EMPTY}</td>
      <td>{audioText(camera)}</td>
      <td className="mono-cell">{stats ? lastPacketText(stats) : EMPTY}</td>
      <td className="num">{stats && stats.currentFile !== '' ? formatBytes(stats.currentFileBytes) : EMPTY}</td>
    </tr>
  );
}

function HealthHero({ health, cameras }: { health: Health; cameras: Camera[] | null }) {
  const enabled = cameras?.filter((camera) => camera.enabled) ?? [];
  const recording = enabled.filter((camera) => camera.stats?.state === 'recording').length;
  const label = { healthy: 'Healthy', warning: 'Warning', critical: 'Critical' }[health.status] ?? health.status;

  return (
    <section className="hero" aria-labelledby="status-heading">
      <div className="hero-main">
        <h2 id="status-heading" className="visually-hidden">
          Overall status
        </h2>
        <span className={`hero-badge hero-badge-${health.status}`}>{label}</span>
        {cameras ? (
          <p className="hero-count">
            <span className="hero-number">{recording}</span>
            <span className="hero-of">
              / {enabled.length} {enabled.length === 1 ? 'camera' : 'cameras'} recording
            </span>
          </p>
        ) : null}
        <p className="hero-meta">
          Checked at {formatTime(health.checkedAt)} · version {health.version} · up {formatDuration(health.uptimeSec)}
        </p>
      </div>
      <ul className="hero-reasons" aria-label="Reasons">
        <li className="hero-reasons-title">{health.reasons.length > 0 ? 'Needs attention' : 'All good'}</li>
        {health.reasons.length > 0 ? (
          health.reasons.map((reason, index) => (
            <li key={`${reason.code}-${reason.cameraId ?? ''}-${index}`} className={`hero-reason hero-reason-${reason.severity}`}>
              <span>{reason.message}</span>
            </li>
          ))
        ) : (
          <li className="hero-reason">
            <span>Every check passed.</span>
          </li>
        )}
      </ul>
    </section>
  );
}

function StatTiles({ data, health, now }: { data: DestinationsResponse; health: Health | null; now: number }) {
  const staging = data.staging;
  const measured = staging.freeBytes !== null && staging.totalBytes !== null && staging.totalBytes > 0;
  const freeShare = measured ? (staging.freeBytes ?? 0) / (staging.totalBytes ?? 1) : 0;
  const belowFloor = staging.freeBytes !== null && staging.freeBytes < staging.floorBytes;

  const enabled = data.destinations.filter((destination) => destination.enabled);
  const online = enabled.filter((destination) => destination.status.state === 'ok').length;
  const queued = data.destinations.reduce((sum, destination) => sum + destination.status.queueLength, 0);
  const oldest = data.destinations
    .map((destination) => destination.status.oldestQueuedTs)
    .filter((ts): ts is string => ts !== null)
    .sort()[0];

  return (
    <section className="stat-grid" aria-label="Storage at a glance">
      <div className={belowFloor ? 'stat stat-bad' : 'stat'}>
        <span className="stat-label">Staging free</span>
        <span className="stat-value">{measured ? formatBytes(staging.freeBytes) : EMPTY}</span>
        {measured ? (
          <div className={belowFloor ? 'meter meter-warn' : 'meter'} aria-hidden="true">
            <div className="meter-fill" style={{ width: `${Math.round((1 - freeShare) * 100)}%` }} />
          </div>
        ) : null}
        <span className="stat-note">
          {measured ? `of ${formatBytes(staging.totalBytes)} · floor ${formatBytes(staging.floorBytes)}` : 'Not measured yet'}
        </span>
      </div>
      <div className="stat">
        <span className="stat-label">Copy queue</span>
        <span className="stat-value">{queued === 1 ? '1 file' : `${queued} files`}</span>
        <span className="stat-note">{oldest ? `Oldest waiting ${formatAge(oldest, now)}` : 'Nothing waiting'}</span>
      </div>
      <div className={online < enabled.length ? 'stat stat-bad' : 'stat'}>
        <span className="stat-label">Destinations online</span>
        <span className="stat-value">
          {online} of {enabled.length}
        </span>
        <span className="stat-note">{enabled.length === 0 ? 'Recordings stay in staging' : 'Enabled destinations'}</span>
      </div>
      <div className="stat stat-accent">
        <span className="stat-label">Running for</span>
        <span className="stat-value">{health ? formatDuration(health.uptimeSec) : EMPTY}</span>
        <span className="stat-note">{health ? `Version ${health.version}` : ' '}</span>
      </div>
    </section>
  );
}

function DestinationCard({ destination, now }: { destination: Destination; now: number }) {
  const status = destination.status;
  const capBytes = destination.capGB * 1e9;
  const share = capBytes > 0 ? Math.min(1, status.usedBytes / capBytes) : 0;
  return (
    <Link to="/destinations" className="card card-link">
      <div className="card-head">
        <div>
          <h3 className="card-title">{destination.name}</h3>
          <div className="card-sub">{destination.path}</div>
        </div>
        <DestinationStateBadge state={status.state} />
      </div>
      <div className="meter" aria-hidden="true">
        <div className="meter-fill" style={{ width: `${Math.round(share * 100)}%` }} />
      </div>
      <div className="card-foot">
        <span>
          <strong>{formatBytes(status.usedBytes)}</strong> of {formatGB(destination.capGB)} used
        </span>
        <span>
          {status.queueLength} in queue
          {status.queueLength > 0 && status.oldestQueuedTs ? ` · oldest ${formatAge(status.oldestQueuedTs, now)}` : ''}
        </span>
      </div>
      {status.lastError !== '' ? <div className="cell-note">{status.lastError}</div> : null}
    </Link>
  );
}

export function DashboardPage() {
  usePageTitle('Dashboard');
  const health = useLoad(api.health, REFRESH_MS);
  const cameras = useLoad(api.cameras, REFRESH_MS);
  const destinations = useLoad(api.destinations, REFRESH_MS);
  const now = useNow();
  const [filter, setFilter] = useState<CameraFilter>('all');

  const cameraList = cameras.data?.cameras ?? [];
  const attention = cameraList.filter(needsAttention);
  const recording = cameraList.filter((camera) => camera.stats?.state === 'recording');
  const shown = filter === 'attention' ? attention : filter === 'recording' ? recording : cameraList;

  return (
    <>
      <PageHeader eyebrow="Overview" title="Dashboard">
        <span className="live-dot">Refreshes every 5 seconds</span>
        <Link to="/live" className="button button-primary">
          <Icon name="live" size={18} />
          Open live view
        </Link>
      </PageHeader>

      {health.error ? <Alert kind="error">Status could not be refreshed: {health.error}</Alert> : null}
      {health.data ? <HealthHero health={health.data} cameras={cameras.data?.cameras ?? null} /> : null}
      {!health.data && health.loading ? <p className="muted">Loading…</p> : null}

      {destinations.data ? <StatTiles data={destinations.data} health={health.data} now={now} /> : null}

      <section className="section" aria-labelledby="cameras-heading">
        <div className="section-header">
          <h2 id="cameras-heading">Cameras</h2>
          <Segmented<CameraFilter>
            label="Show cameras"
            value={filter}
            onChange={setFilter}
            options={[
              { value: 'all', label: <>All <span className="seg-count">{cameraList.length}</span></> },
              { value: 'attention', label: <>Needs attention <span className="seg-count">{attention.length}</span></> },
              { value: 'recording', label: <>Recording <span className="seg-count">{recording.length}</span></> },
            ]}
          />
        </div>
        {cameras.error ? <Alert kind="error">Cameras could not be refreshed: {cameras.error}</Alert> : null}
        {cameras.data ? (
          cameraList.length === 0 ? (
            <p>
              No cameras have been added yet. <Link to="/cameras">Add a camera</Link>
            </p>
          ) : shown.length === 0 ? (
            <p className="muted">{filter === 'attention' ? 'Every enabled camera is recording.' : 'No camera is recording.'}</p>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Camera</th>
                    <th scope="col">State</th>
                    <th scope="col">Video</th>
                    <th scope="col" className="num">
                      Bitrate
                    </th>
                    <th scope="col">Audio</th>
                    <th scope="col">Last packet</th>
                    <th scope="col" className="num">
                      Current file
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map((camera) => (
                    <CameraRow key={camera.id} camera={camera} />
                  ))}
                </tbody>
              </table>
            </div>
          )
        ) : cameras.loading ? (
          <p className="muted">Loading…</p>
        ) : null}
      </section>

      <section className="section" aria-labelledby="destinations-heading">
        <div className="section-header">
          <h2 id="destinations-heading">Destinations</h2>
        </div>
        {destinations.error ? (
          <Alert kind="error">Destinations could not be refreshed: {destinations.error}</Alert>
        ) : null}
        {destinations.data ? (
          destinations.data.destinations.length > 0 ? (
            <div className="card-grid">
              {destinations.data.destinations.map((destination) => (
                <DestinationCard key={destination.id} destination={destination} now={now} />
              ))}
            </div>
          ) : (
            <p>No destinations have been added yet. Recordings stay in staging.</p>
          )
        ) : destinations.loading ? (
          <p className="muted">Loading…</p>
        ) : null}
        {destinations.data ? (
          <p className="page-footer">
            Staging folder <span className="mono">{destinations.data.staging.path}</span> ·{' '}
            {formatBytes(destinations.data.staging.usedBytes)} of recordings waiting or kept there
          </p>
        ) : null}
      </section>
    </>
  );
}
