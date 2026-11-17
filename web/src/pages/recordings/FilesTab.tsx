import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { api, errorMessage } from '../../api/client';
import type { Segment } from '../../api/types';
import { Alert } from '../../components/Alert';
import { Badge, CopyStateBadge } from '../../components/Badge';
import { SelectField, TextField } from '../../components/Fields';
import { Icon } from '../../components/Icon';
import { EMPTY, formatBytes, formatCodec, formatDateTime, humanize, todayLocal } from '../../lib/format';
import { useLoad } from '../../lib/hooks';

const CLOSE_REASONS: Record<string, string> = {
  hour_rollover: 'Hour rollover',
  stall: 'Stream stalled',
  shutdown: 'Camera or service stopped',
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

type HourMark = 'none' | 'full' | 'gap' | 'now';

/** One mark per local hour of the chosen day: recorded, recorded with a gap, being written, or nothing. */
function hourMarks(segments: Segment[], date: string): HourMark[] {
  const marks: HourMark[] = Array.from({ length: 24 }, () => 'none');
  const dayStart = new Date(`${date}T00:00:00`).getTime();
  if (Number.isNaN(dayStart)) return marks;
  for (const segment of segments) {
    const start = Date.parse(segment.start);
    const end = segment.end === null ? Date.now() : Date.parse(segment.end);
    if (Number.isNaN(start) || Number.isNaN(end)) continue;
    const first = Math.max(0, Math.floor((start - dayStart) / 3_600_000));
    const last = Math.min(23, Math.floor((Math.max(start, end - 1) - dayStart) / 3_600_000));
    for (let hour = first; hour <= last; hour++) {
      if (marks[hour] === 'none') marks[hour] = 'full';
    }
    if (segment.gapBefore && first <= 23 && marks[first] !== 'now') marks[first] = 'gap';
    if (segment.end === null && last >= 0) marks[last] = 'now';
  }
  return marks;
}

function HourStrip({ segments, date }: { segments: Segment[]; date: string }) {
  const marks = hourMarks(segments, date);
  const recorded = marks.filter((mark) => mark !== 'none').length;
  const gaps = marks.filter((mark) => mark === 'gap').length;
  return (
    <section className="hour-strip" aria-labelledby="day-heading">
      <div className="hour-strip-head">
        <h2 id="day-heading">{new Date(`${date}T12:00:00`).toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' })}</h2>
        <ul className="legend">
          <li>Recorded</li>
          <li style={{ '--key': 'var(--orange)' } as CSSProperties}>Starts after a gap</li>
          <li style={{ '--key': 'var(--butter)' } as CSSProperties}>Recording now</li>
        </ul>
      </div>
      <div
        className="hours"
        role="img"
        aria-label={`${recorded} of 24 hours have recordings${gaps > 0 ? `, ${gaps} of them after a gap` : ''}.`}
      >
        {marks.map((mark, hour) => (
          <div key={hour} className={mark === 'none' ? 'hour' : `hour hour-${mark}`} title={`${String(hour).padStart(2, '0')}:00`} />
        ))}
      </div>
      <div className="hour-axis" aria-hidden="true">
        <span>00:00</span>
        <span>06:00</span>
        <span>12:00</span>
        <span>18:00</span>
        <span>24:00</span>
      </div>
    </section>
  );
}

/** The v1 file list of the Recordings page: one row per hour file, with copies and download. */
export function FilesTab() {
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

      {cameraId !== '' && !loading && segments !== null && segments.length > 0 && dateError === undefined ? (
        <HourStrip segments={segments} date={date} />
      ) : null}

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
                        <a className="button button-small" href={api.segmentDownloadUrl(segment.id)} download={segment.fileName}>
                          <Icon name="download" size={16} />
                          Download
                        </a>
                      ) : (
                        <span className="muted">
                          {segment.end === null ? 'Can be downloaded once the file is closed' : 'No copy left to download'}
                        </span>
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
