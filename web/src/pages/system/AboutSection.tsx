import type { SystemInfo } from '../../api/types';
import { formatDateTime, formatDuration } from '../../lib/format';

export function AboutSection({ system }: { system: SystemInfo }) {
  const counts = system.counts;
  return (
    <section className="panel" aria-labelledby="about-heading">
      <h2 id="about-heading">Version and uptime</h2>
      <dl className="facts">
        <div>
          <dt>Version</dt>
          <dd>{system.version}</dd>
        </div>
        <div>
          <dt>Uptime</dt>
          <dd>
            {formatDuration(system.uptimeSec)} (started {formatDateTime(system.startedAt)})
          </dd>
        </div>
        <div>
          <dt>Server</dt>
          <dd>
            {system.hostname} ({system.os})
          </dd>
        </div>
        <div>
          <dt>Data folder</dt>
          <dd className="mono">{system.dataRoot}</dd>
        </div>
        <div>
          <dt>Config schema</dt>
          <dd>Version {system.schemaVersion}</dd>
        </div>
        <div>
          <dt>Cameras</dt>
          <dd>
            {counts.enabledCameras} enabled of {counts.cameras}, on{' '}
            {counts.workers === 1 ? '1 recorder worker' : `${counts.workers} recorder workers`}
          </dd>
        </div>
        <div>
          <dt>Destinations</dt>
          <dd>{counts.destinations}</dd>
        </div>
      </dl>
    </section>
  );
}
