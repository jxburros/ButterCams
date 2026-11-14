import { useCallback, useEffect, useRef, useState } from 'react';
import { api, errorMessage } from '../api/client';
import type { EventRow, EventsQuery, Severity } from '../api/types';
import { Alert } from '../components/Alert';
import { SeverityBadge } from '../components/Badge';
import { SelectField, TextField } from '../components/Fields';
import { Icon } from '../components/Icon';
import { Segmented } from '../components/PageHeader';
import { EMPTY, formatDateTime, humanize } from '../lib/format';
import { useLoad, usePageTitle } from '../lib/hooks';

const PAGE_SIZE = 200;

interface Filters {
  severity: '' | Severity;
  camera: string;
  /** Value of a datetime-local input (local time), or "" for no lower limit. */
  since: string;
}

const NO_FILTERS: Filters = { severity: '', camera: '', since: '' };

function toSeverity(value: string): '' | Severity {
  return value === 'info' || value === 'warning' || value === 'critical' ? value : '';
}

/** The datetime-local value as an RFC 3339 timestamp, or `null` when it is not a date. */
function sinceTimestamp(since: string): string | null {
  const ms = Date.parse(since);
  return Number.isNaN(ms) ? null : new Date(ms).toISOString();
}

/** The text of an event: its `data.message`, or the raw data when there is none. */
function eventMessage(event: EventRow): string {
  const data = event.data;
  if (data === null || data === undefined) return EMPTY;
  if (typeof data.message === 'string' && data.message !== '') return data.message;
  const text = JSON.stringify(data);
  return text === '{}' ? EMPTY : text;
}

export function EventsPage() {
  usePageTitle('Events');
  const cameras = useLoad(api.recordingCameras);

  const [filters, setFilters] = useState<Filters>(NO_FILTERS);
  const [events, setEvents] = useState<EventRow[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const requestRef = useRef(0);

  const sinceInvalid = filters.since !== '' && sinceTimestamp(filters.since) === null;

  const buildQuery = useCallback((active: Filters, before?: number): EventsQuery => {
    const query: EventsQuery = { limit: PAGE_SIZE };
    if (active.severity !== '') query.severity = active.severity;
    if (active.camera !== '') query.camera = active.camera;
    const since = active.since === '' ? null : sinceTimestamp(active.since);
    if (since !== null) query.since = since;
    if (before !== undefined) query.before = before;
    return query;
  }, []);

  /** Loads the newest events for the given filters, replacing the table. */
  const loadFirstPage = useCallback(
    async (active: Filters) => {
      requestRef.current += 1;
      const request = requestRef.current;
      setLoading(true);
      setFailure(null);
      try {
        const response = await api.events(buildQuery(active));
        if (request !== requestRef.current) return;
        setEvents(response.events);
        setHasMore(response.events.length >= PAGE_SIZE);
      } catch (err) {
        if (request !== requestRef.current) return;
        setFailure(errorMessage(err));
      } finally {
        if (request === requestRef.current) setLoading(false);
      }
    },
    [buildQuery],
  );

  useEffect(() => {
    if (filters.since !== '' && sinceTimestamp(filters.since) === null) return;
    // Rows loaded for the previous filters no longer apply.
    setEvents([]);
    setHasMore(false);
    void loadFirstPage(filters);
  }, [filters, loadFirstPage]);

  /** Adds the next older page below the events already shown. */
  async function loadMore() {
    const oldest = events[events.length - 1];
    if (oldest === undefined) return;
    requestRef.current += 1;
    const request = requestRef.current;
    setLoadingMore(true);
    setFailure(null);
    try {
      const response = await api.events(buildQuery(filters, oldest.id));
      if (request !== requestRef.current) return;
      setEvents((current) => [...current, ...response.events]);
      setHasMore(response.events.length >= PAGE_SIZE);
    } catch (err) {
      if (request !== requestRef.current) return;
      setFailure(errorMessage(err));
    } finally {
      setLoadingMore(false);
    }
  }

  const cameraList = cameras.data?.cameras ?? [];
  const cameraName = (id: string): string => cameraList.find((camera) => camera.id === id)?.name ?? id;

  return (
    <>
      <div className="page-header">
        <div className="page-title">
          <p className="eyebrow">Everything the service noticed</p>
          <h1>Events</h1>
        </div>
        <div className="button-row">
          <button
            type="button"
            className="button"
            disabled={loading || sinceInvalid}
            onClick={() => void loadFirstPage(filters)}
          >
            <Icon name="refresh" size={18} />
            Refresh
          </button>
        </div>
      </div>

      <section className="panel" aria-label="Filters">
        <div className="filter-row">
          <div className="seg-field">
            <span className="seg-field-label">Severity</span>
            <Segmented<'' | Severity>
              label="Severity"
              value={filters.severity}
              onChange={(severity) => setFilters((current) => ({ ...current, severity: toSeverity(severity) }))}
              options={[
                { value: '', label: 'All' },
                { value: 'info', label: 'Info' },
                { value: 'warning', label: 'Warning' },
                { value: 'critical', label: 'Critical' },
              ]}
            />
          </div>
          <SelectField
            label="Camera"
            value={filters.camera}
            onChange={(camera) => setFilters((current) => ({ ...current, camera }))}
          >
            <option value="">All</option>
            {cameraList.map((camera) => (
              <option key={camera.id} value={camera.id}>
                {camera.deleted ? `${camera.name} (deleted camera)` : camera.name}
              </option>
            ))}
          </SelectField>
          <TextField
            label="Since"
            type="datetime-local"
            value={filters.since}
            onChange={(since) => setFilters((current) => ({ ...current, since }))}
            error={sinceInvalid ? 'Enter a complete date and time, or clear this box.' : undefined}
          />
          <button
            type="button"
            className="button"
            onClick={() => setFilters(NO_FILTERS)}
            disabled={filters.severity === '' && filters.camera === '' && filters.since === ''}
          >
            Clear filters
          </button>
        </div>
        {cameras.error ? <p className="field-error">The camera list could not be loaded: {cameras.error}</p> : null}
      </section>

      {failure ? <Alert kind="error">Events could not be loaded: {failure}</Alert> : null}

      <section className="panel" aria-labelledby="events-heading">
        <h2 id="events-heading" className="visually-hidden">
          Event list
        </h2>
        {loading && events.length === 0 ? (
          <p className="muted">Loading…</p>
        ) : events.length === 0 ? (
          failure ? null : (
            <p>No events match these filters.</p>
          )
        ) : (
          <>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Time</th>
                    <th scope="col">Severity</th>
                    <th scope="col">Source</th>
                    <th scope="col">Type</th>
                    <th scope="col">Camera</th>
                    <th scope="col">Details</th>
                  </tr>
                </thead>
                <tbody>
                  {events.map((event) => (
                    <tr key={event.id}>
                      <td className="nowrap">{formatDateTime(event.ts)}</td>
                      <td>
                        <SeverityBadge severity={event.severity} />
                      </td>
                      <td className="mono-cell">{event.source}</td>
                      <td>{humanize(event.type)}</td>
                      <td>{event.cameraId ? cameraName(event.cameraId) : EMPTY}</td>
                      <td className="wrap-anywhere">{eventMessage(event)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="button-row table-footer">
              {hasMore ? (
                <button type="button" className="button" disabled={loadingMore || loading} onClick={() => void loadMore()}>
                  {loadingMore ? 'Loading…' : 'Load more'}
                </button>
              ) : (
                <span className="muted">
                  {events.length === 1 ? '1 event. ' : `${events.length} events. `}There are no older events for these
                  filters.
                </span>
              )}
            </div>
          </>
        )}
      </section>
    </>
  );
}
