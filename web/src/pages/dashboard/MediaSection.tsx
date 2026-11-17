import { Link } from 'react-router-dom';
import { api } from '../../api/client';
import type { Camera, MediaStatus } from '../../api/types';
import { useAuth } from '../../auth/AuthContext';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { formatBytes } from '../../lib/format';
import { REFRESH_MS, useLoad } from '../../lib/hooks';
import '../../live.css';

function stateBadge(media: MediaStatus) {
  if (!media.enabled || media.state === 'off') return <Badge tone="grey">Switched off</Badge>;
  if (media.state === 'running') return <Badge tone="green">Running</Badge>;
  return <Badge tone="amber">Restarting</Badge>;
}

const SOURCE_STATE: Record<string, string> = {
  playing: 'playing',
  connecting: 'connecting',
  waiting: 'waiting for the camera',
};

/**
 * The live view process on the Dashboard (spec 16.8): its state, how many
 * people are watching and which camera streams are open for them.
 */
export function MediaSection({ cameras }: { cameras: Camera[] }) {
  const { isAdmin } = useAuth();
  const system = useLoad(api.system, REFRESH_MS);
  const media = system.data?.media ?? null;
  const name = (id: string) => cameras.find((camera) => camera.id === id)?.name ?? id;

  return (
    <section className="section" aria-labelledby="media-heading">
      <div className="section-header">
        <h2 id="media-heading">Live view</h2>
      </div>
      {system.error ? <Alert kind="error">Live view status could not be refreshed: {system.error}</Alert> : null}
      {media === null ? (
        system.loading ? (
          <p className="muted">Loading…</p>
        ) : null
      ) : (
        <div className="panel">
          <dl className="facts">
            <div>
              <dt>Live view process</dt>
              <dd>
                {stateBadge(media)}
                {media.state === 'starting' && media.enabled ? (
                  <span className="cell-note"> Viewers reconnect by themselves. Recording is not affected.</span>
                ) : null}
                {!media.enabled ? (
                  <span className="cell-note">
                    {' '}
                    Live view and playback are switched off
                    {isAdmin ? (
                      <>
                        {' '}
                        on the <Link to="/system">System page</Link>
                      </>
                    ) : null}
                    .
                  </span>
                ) : null}
              </dd>
            </div>
            {media.state === 'running' ? (
              <>
                <div>
                  <dt>Watching now</dt>
                  <dd>
                    {media.viewers === 1 ? '1 live stream' : `${media.viewers} live streams`} open in browsers
                    {media.overflows > 0 ? ` · ${media.overflows} times a slow viewer had to skip ahead` : ''}
                  </dd>
                </div>
                <div>
                  <dt>Memory</dt>
                  <dd>{formatBytes(media.memBytes)}</dd>
                </div>
                <div>
                  <dt>Camera streams open</dt>
                  <dd>
                    {media.sources.length === 0 ? (
                      media.viewers === 0 ? (
                        'None. Streams open when someone watches and close 30 seconds after the last viewer leaves.'
                      ) : (
                        'Not listed for this account.'
                      )
                    ) : (
                      <ul className="media-sources">
                        {media.sources.map((source) => (
                          <li key={`${source.cameraId}/${source.stream}`}>
                            <span>
                              {name(source.cameraId)} · {source.stream === 'main' ? 'main stream' : 'sub stream'}
                            </span>
                            <span className="cell-note">
                              {source.viewers === 1 ? '1 viewer' : `${source.viewers} viewers`} ·{' '}
                              {SOURCE_STATE[source.state] ?? source.state}
                            </span>
                          </li>
                        ))}
                      </ul>
                    )}
                  </dd>
                </div>
              </>
            ) : null}
          </dl>
        </div>
      )}
    </section>
  );
}
