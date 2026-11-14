import { api } from '../api/client';
import type { Camera, Destination } from '../api/types';
import { Alert } from '../components/Alert';
import { CameraStateBadge, DestinationStateBadge, HealthBadge, SeverityBadge } from '../components/Badge';
import { StagingSummary } from '../components/StagingSummary';
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

function CameraRow({ camera, now }: { camera: Camera; now: number }) {
  const stats = camera.stats;
  return (
    <tr>
      <th scope="row">{camera.name}</th>
      <td>
        <CameraStateBadge state={stats?.state ?? (camera.enabled ? null : 'disabled')} />
        {stats && stats.message !== '' ? <div className="cell-note">{stats.message}</div> : null}
      </td>
      <td className="num">{stats ? formatBitrate(stats.bitrateKbps) : EMPTY}</td>
      <td>{stats ? formatResolution(stats.width, stats.height) : EMPTY}</td>
      <td>{stats ? formatCodec(stats.videoCodec) : EMPTY}</td>
      <td>{audioText(camera)}</td>
      <td>{stats ? formatAge(stats.lastPacketTs, now) : EMPTY}</td>
      <td className="num">{stats && stats.currentFile !== '' ? formatBytes(stats.currentFileBytes) : EMPTY}</td>
    </tr>
  );
}

function DestinationRow({ destination, now }: { destination: Destination; now: number }) {
  const status = destination.status;
  return (
    <tr>
      <th scope="row">{destination.name}</th>
      <td>
        <DestinationStateBadge state={status.state} />
        {status.lastError !== '' ? <div className="cell-note">{status.lastError}</div> : null}
      </td>
      <td>
        {formatBytes(status.usedBytes)} / {formatGB(destination.capGB)}
      </td>
      <td>
        {status.queueLength}
        {status.queueLength > 0 && status.oldestQueuedTs ? (
          <span className="cell-note"> (oldest queued {formatAge(status.oldestQueuedTs, now)})</span>
        ) : null}
      </td>
    </tr>
  );
}

export function DashboardPage() {
  usePageTitle('Dashboard');
  const health = useLoad(api.health, REFRESH_MS);
  const cameras = useLoad(api.cameras, REFRESH_MS);
  const destinations = useLoad(api.destinations, REFRESH_MS);
  const now = useNow();

  return (
    <>
      <div className="page-header">
        <h1>Dashboard</h1>
        <p className="muted">Refreshes every 5 seconds.</p>
      </div>

      <section className="panel" aria-labelledby="status-heading">
        <h2 id="status-heading">Overall status</h2>
        {health.error ? <Alert kind="error">Status could not be refreshed: {health.error}</Alert> : null}
        {health.data ? (
          <>
            <p className="status-line">
              <HealthBadge status={health.data.status} />
              <span className="muted">
                Checked at {formatTime(health.data.checkedAt)} · version {health.data.version} · up{' '}
                {formatDuration(health.data.uptimeSec)}
              </span>
            </p>
            {health.data.reasons.length > 0 ? (
              <ul className="reason-list">
                {health.data.reasons.map((reason, index) => (
                  <li key={`${reason.code}-${reason.cameraId ?? ''}-${index}`}>
                    <SeverityBadge severity={reason.severity} /> {reason.message}
                  </li>
                ))}
              </ul>
            ) : (
              <p>Every check passed.</p>
            )}
          </>
        ) : health.loading ? (
          <p className="muted">Loading…</p>
        ) : null}
      </section>

      <section className="panel" aria-labelledby="cameras-heading">
        <h2 id="cameras-heading">Cameras</h2>
        {cameras.error ? <Alert kind="error">Cameras could not be refreshed: {cameras.error}</Alert> : null}
        {cameras.data ? (
          cameras.data.cameras.length > 0 ? (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">State</th>
                    <th scope="col" className="num">
                      Bitrate
                    </th>
                    <th scope="col">Resolution</th>
                    <th scope="col">Codec</th>
                    <th scope="col">Audio</th>
                    <th scope="col">Last packet</th>
                    <th scope="col" className="num">
                      Current file
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {cameras.data.cameras.map((camera) => (
                    <CameraRow key={camera.id} camera={camera} now={now} />
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p>No cameras have been added yet.</p>
          )
        ) : cameras.loading ? (
          <p className="muted">Loading…</p>
        ) : null}
      </section>

      <section className="panel" aria-labelledby="destinations-heading">
        <h2 id="destinations-heading">Destinations</h2>
        {destinations.error ? (
          <Alert kind="error">Destinations could not be refreshed: {destinations.error}</Alert>
        ) : null}
        {destinations.data ? (
          destinations.data.destinations.length > 0 ? (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Status</th>
                    <th scope="col">Used / limit</th>
                    <th scope="col">Queue</th>
                  </tr>
                </thead>
                <tbody>
                  {destinations.data.destinations.map((destination) => (
                    <DestinationRow key={destination.id} destination={destination} now={now} />
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p>No destinations have been added yet. Recordings stay in staging.</p>
          )
        ) : destinations.loading ? (
          <p className="muted">Loading…</p>
        ) : null}
      </section>

      <section className="panel" aria-labelledby="staging-heading">
        <h2 id="staging-heading">Staging</h2>
        {destinations.data ? <StagingSummary staging={destinations.data.staging} /> : null}
        {!destinations.data && destinations.loading ? <p className="muted">Loading…</p> : null}
      </section>
    </>
  );
}
