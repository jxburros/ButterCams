import { useState } from 'react';
import { api, errorMessage } from '../api/client';
import type { Destination } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { Alert } from '../components/Alert';
import { DestinationStateBadge } from '../components/Badge';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { StagingSummary } from '../components/StagingSummary';
import { formatAge, formatBytes, formatDateTime, formatGB } from '../lib/format';
import { REFRESH_MS, useLoad, useNow, usePageTitle } from '../lib/hooks';
import { DestinationForm } from './DestinationForm';

type View = { kind: 'list' } | { kind: 'add' } | { kind: 'edit'; destination: Destination };

const TYPE_LABELS: Record<Destination['type'], string> = {
  local: 'Local drive',
  unc: 'Network share',
};

export function DestinationsPage() {
  usePageTitle('Destinations');
  const { isAdmin } = useAuth();
  const destinations = useLoad(api.destinations, REFRESH_MS);
  const now = useNow();

  const [view, setView] = useState<View>({ kind: 'list' });
  const [notice, setNotice] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<Destination | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const list = destinations.data?.destinations ?? [];

  function open(next: View) {
    setNotice(null);
    setView(next);
  }

  async function confirmDelete() {
    if (deleting === null) return;
    setDeleteBusy(true);
    setDeleteError(null);
    try {
      await api.deleteDestination(deleting.id);
      setDeleting(null);
      setNotice(`${deleting.name} was deleted.`);
      await destinations.reload();
    } catch (err) {
      // Includes the 409 the server sends while cameras still use the destination.
      setDeleteError(errorMessage(err));
    } finally {
      setDeleteBusy(false);
    }
  }

  if (isAdmin && view.kind !== 'list') {
    const editing = view.kind === 'edit' ? view.destination : null;
    return (
      <>
        <div className="page-header">
          <h1>Destinations</h1>
        </div>
        <DestinationForm
          key={editing?.id ?? 'new'}
          destination={editing}
          onCancel={() => open({ kind: 'list' })}
          onSaved={(saved) => {
            setView({ kind: 'list' });
            setNotice(editing ? `${saved.name} was saved.` : `${saved.name} was added.`);
            void destinations.reload();
          }}
        />
      </>
    );
  }

  return (
    <>
      <div className="page-header">
        <h1>Destinations</h1>
        {isAdmin ? (
          <div className="button-row">
            <button type="button" className="button button-primary" onClick={() => open({ kind: 'add' })}>
              Add destination
            </button>
          </div>
        ) : null}
      </div>

      {notice ? <Alert kind="success">{notice}</Alert> : null}
      {destinations.error ? (
        <Alert kind="error">Destinations could not be loaded: {destinations.error}</Alert>
      ) : null}

      <section className="panel">
        {destinations.data === null ? (
          destinations.loading ? (
            <p className="muted">Loading…</p>
          ) : null
        ) : list.length === 0 ? (
          <p>No destinations have been added yet. Recordings stay in staging until a camera is given one.</p>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Path</th>
                  <th scope="col">Status</th>
                  <th scope="col">Used / limit</th>
                  <th scope="col">Queue</th>
                  <th scope="col">Max age</th>
                  {isAdmin ? <th scope="col">Actions</th> : null}
                </tr>
              </thead>
              <tbody>
                {list.map((destination) => {
                  const status = destination.status;
                  return (
                    <tr key={destination.id}>
                      <th scope="row">
                        {destination.name}
                        <div className="cell-note mono">{destination.id}</div>
                      </th>
                      <td className="cell-path">
                        <span className="mono wrap-anywhere">{destination.path}</span>
                        <div className="cell-note">{TYPE_LABELS[destination.type] ?? destination.type}</div>
                      </td>
                      <td>
                        <DestinationStateBadge state={status.state} />
                        {status.lastError !== '' ? <div className="cell-note">{status.lastError}</div> : null}
                        {status.lastSuccessTs ? (
                          <div className="cell-note">Last copy succeeded {formatDateTime(status.lastSuccessTs)}</div>
                        ) : null}
                      </td>
                      <td>
                        {formatBytes(status.usedBytes)} / {formatGB(destination.capGB)}
                        {status.freeBytes !== null && status.totalBytes !== null ? (
                          <div className="cell-note">
                            Disk: {formatBytes(status.freeBytes)} free of {formatBytes(status.totalBytes)}
                          </div>
                        ) : null}
                      </td>
                      <td>
                        {status.queueLength === 1 ? '1 file' : `${status.queueLength} files`}
                        {status.queueLength > 0 && status.oldestQueuedTs ? (
                          <div className="cell-note">Oldest queued {formatAge(status.oldestQueuedTs, now)}</div>
                        ) : null}
                      </td>
                      <td>
                        {destination.maxAgeDays === null
                          ? 'No limit'
                          : destination.maxAgeDays === 1
                            ? '1 day'
                            : `${destination.maxAgeDays} days`}
                      </td>
                      {isAdmin ? (
                        <td>
                          <div className="button-row">
                            <button
                              type="button"
                              className="button button-small"
                              onClick={() => open({ kind: 'edit', destination })}
                              aria-label={`Edit ${destination.name}`}
                            >
                              Edit
                            </button>
                            <button
                              type="button"
                              className="button button-small button-danger"
                              onClick={() => {
                                setDeleteError(null);
                                setDeleting(destination);
                              }}
                              aria-label={`Delete ${destination.name}`}
                            >
                              Delete
                            </button>
                          </div>
                        </td>
                      ) : null}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="panel" aria-labelledby="staging-heading">
        <h2 id="staging-heading">Staging</h2>
        <p className="muted">
          Every camera records into staging first; closed hour files are then copied to the camera's destinations.
          {isAdmin ? ' The staging folder and its floor are set on the System page.' : ''}
        </p>
        {destinations.data ? <StagingSummary staging={destinations.data.staging} /> : null}
      </section>

      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          confirmLabel="Delete destination"
          danger
          busy={deleteBusy}
          error={deleteError}
          onConfirm={() => void confirmDelete()}
          onCancel={() => setDeleting(null)}
        >
          <p>
            ButterCams stops copying recordings to <span className="mono">{deleting.path}</span>. A destination that
            cameras still use can't be deleted; remove it from those cameras first.
          </p>
        </ConfirmDialog>
      ) : null}
    </>
  );
}
