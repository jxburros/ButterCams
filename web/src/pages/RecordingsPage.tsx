import { useEffect, useState } from 'react';
import { api, errorMessage } from '../api/client';
import type { Segment } from '../api/types';
import { Alert } from '../components/Alert';
import { Badge, CopyStateBadge } from '../components/Badge';
import { SelectField, TextField } from '../components/Fields';
import { EMPTY, formatBytes, formatCodec, formatDateTime, humanize, todayLocal } from '../lib/format';
import { useLoad, usePageTitle } from '../lib/hooks';

const CLOSE_REASONS: Record<string, string> = {
  hour_rollover: 'Hour rollover',
  stall: 'Stream stalled',
  shutdown: 'Service shutdown',
  crash_recovered: 'Recovered after a crash',
  param_change: 'Stream settings changed',
  clock_jump: 'Clock change',
};

function closeReasonText(segment: Segment): string {
  if (!segment.closeReason) return segment.end === null ? 'Still recording' : EMPTY;
  return CLOSE_REASONS[segment.closeReason] ?? humanize(segment.closeReason);
}

/** A file can be downloaded while at least one copy of it still exists. */
function hasCopy(segment: Segment): boolean {
  return segment.copies.some((copy) => copy.state !== 'deleted');
}

function isDate(text: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(text) && !Number.isNaN(Date.parse(`${text}T00:00:00`));
}

export function RecordingsPage() {
  usePageTitle('Recordings');
  const cameras = useLoad(api.recordingCameras);
  const [cameraId, setCameraId] = useState('');
  const [date, setDate] = useState(todayLocal);

  const [segments, setSegments] = useState<Segment[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const dateError = isDate(date) ? undefined : 'Choose a date.';

  useEffect(() => {
    if (cameraId === '' || !isDate(date)) {
      setSegments(null);
      setFailure(null);
      setLoading(false);
      return;
    }
    let current = true;
    setSegments(null);
    setLoading(true);
    setFailure(null);
    api
      .segments(cameraId, date)
      .then((response) => {
        if (current) setSegments(response.segments);
      })
      .catch((err: unknown) => {
        if (current) {
          setSegments(null);
          setFailure(errorMessage(err));
        }
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [cameraId, date]);

  const cameraList = cameras.data?.cameras ?? [];

  return (
    <>
      <div className="page-header">
        <h1>Recordings</h1>
      </div>

      {cameras.error ? <Alert kind="error">The camera list could not be loaded: {cameras.error}</Alert> : null}

      <section className="panel" aria-label="Choose recordings">
        <div className="filter-row">
          <SelectField label="Camera" value={cameraId} onChange={setCameraId} disabled={cameras.data === null}>
            <option value="">{cameras.loading ? 'Loading…' : 'Choose a camera'}</option>
            {cameraList.map((camera) => (
              <option key={camera.id} value={camera.id}>
                {camera.deleted ? `${camera.name} (deleted camera)` : camera.name}
              </option>
            ))}
          </SelectField>
          <TextField label="Date" type="date" value={date} onChange={setDate} error={dateError} />
        </div>
      </section>

      {failure ? <Alert kind="error">Recordings could not be loaded: {failure}</Alert> : null}

      <section className="panel" aria-labelledby="files-heading">
        <h2 id="files-heading">Hour files</h2>
        {cameraId === '' ? (
          <p className="muted">Choose a camera to list its recordings for the date.</p>
        ) : loading ? (
          <p className="muted">Loading…</p>
        ) : segments === null ? null : segments.length === 0 ? (
          <p>No recordings were found for this camera on this date.</p>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Start</th>
                  <th scope="col">End</th>
                  <th scope="col" className="num">
                    Size
                  </th>
                  <th scope="col">Gap</th>
                  <th scope="col">Closed by</th>
                  <th scope="col">Copies</th>
                  <th scope="col">File</th>
                </tr>
              </thead>
              <tbody>
                {segments.map((segment) => (
                  <tr key={segment.id}>
                    <td className="nowrap">{formatDateTime(segment.start)}</td>
                    <td className="nowrap">{segment.end === null ? 'Recording now' : formatDateTime(segment.end)}</td>
                    <td className="num">{segment.bytes === null ? EMPTY : formatBytes(segment.bytes)}</td>
                    <td>{segment.gapBefore ? <Badge tone="amber">Gap before</Badge> : 'No'}</td>
                    <td>{closeReasonText(segment)}</td>
                    <td>
                      {segment.copies.length === 0 ? (
                        EMPTY
                      ) : (
                        <ul className="plain-list">
                          {segment.copies.map((copy) => (
                            <li key={copy.destinationId}>
                              <span className="nowrap">
                                <span className="mono">{copy.destinationId}</span> <CopyStateBadge state={copy.state} />
                              </span>
                              {copy.lastError ? <div className="cell-note">{copy.lastError}</div> : null}
                            </li>
                          ))}
                        </ul>
                      )}
                    </td>
                    <td>
                      <div className="mono wrap-anywhere">{segment.fileName}</div>
                      <div className="cell-note">
                        {formatCodec(segment.videoCodec)}
                        {segment.audioCodec ? ` + ${formatCodec(segment.audioCodec)}` : ', no audio'}
                      </div>
                      {hasCopy(segment) ? (
                        <a href={api.segmentDownloadUrl(segment.id)} download={segment.fileName}>
                          Download
                        </a>
                      ) : (
                        <span className="muted">No copy left to download</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}
