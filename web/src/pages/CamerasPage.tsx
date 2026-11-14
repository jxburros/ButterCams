import { useState } from 'react';
import { api, errorMessage } from '../api/client';
import type { Camera } from '../api/types';
import { useAuth } from '../auth/AuthContext';
import { Alert } from '../components/Alert';
import { CameraStateBadge } from '../components/Badge';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { CheckboxField } from '../components/Fields';
import { REFRESH_MS, useLoad, usePageTitle } from '../lib/hooks';
import { CameraForm, cameraToInput } from './CameraForm';
import { CameraImport } from './CameraImport';

type View = { kind: 'list' } | { kind: 'add' } | { kind: 'edit'; camera: Camera } | { kind: 'import' };

export function CamerasPage() {
  usePageTitle('Cameras');
  const { isAdmin } = useAuth();
  const cameras = useLoad(api.cameras, REFRESH_MS);
  const destinations = useLoad(api.destinations);

  const [view, setView] = useState<View>({ kind: 'list' });
  const [notice, setNotice] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);

  const [deleting, setDeleting] = useState<Camera | null>(null);
  const [keepRecordings, setKeepRecordings] = useState(true);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const list = cameras.data?.cameras ?? [];
  const destinationList = destinations.data?.destinations ?? [];

  function destinationNames(camera: Camera): string {
    if (camera.destinationIds.length === 0) return 'Staging only';
    return camera.destinationIds
      .map((id) => destinationList.find((destination) => destination.id === id)?.name ?? id)
      .join(', ');
  }

  function open(next: View) {
    setNotice(null);
    setActionError(null);
    setView(next);
    // Destinations may have changed since this page was opened.
    if (next.kind === 'add' || next.kind === 'edit') void destinations.reload();
  }

  async function toggleEnabled(camera: Camera) {
    setBusyId(camera.id);
    setNotice(null);
    setActionError(null);
    try {
      await api.updateCamera(camera.id, { ...cameraToInput(camera), enabled: !camera.enabled });
      await cameras.reload();
      setNotice(`${camera.name} is now ${camera.enabled ? 'disabled' : 'enabled'}.`);
    } catch (err) {
      setActionError(`${camera.name} could not be ${camera.enabled ? 'disabled' : 'enabled'}: ${errorMessage(err)}`);
    } finally {
      setBusyId(null);
    }
  }

  function askDelete(camera: Camera) {
    setKeepRecordings(true);
    setDeleteError(null);
    setDeleting(camera);
  }

  async function confirmDelete() {
    if (deleting === null) return;
    setDeleteBusy(true);
    setDeleteError(null);
    try {
      await api.deleteCamera(deleting.id, keepRecordings);
      setDeleting(null);
      setActionError(null);
      setNotice(
        keepRecordings
          ? `${deleting.name} was deleted. Its recordings were kept.`
          : `${deleting.name} and its recordings were deleted.`,
      );
      await cameras.reload();
    } catch (err) {
      setDeleteError(errorMessage(err));
    } finally {
      setDeleteBusy(false);
    }
  }

  if (isAdmin && (view.kind === 'add' || view.kind === 'edit')) {
    const editing = view.kind === 'edit' ? view.camera : null;
    return (
      <>
        <div className="page-header">
          <h1>Cameras</h1>
        </div>
        {destinations.error ? (
          <Alert kind="error">The destination list could not be loaded: {destinations.error}</Alert>
        ) : null}
        <CameraForm
          key={editing?.id ?? 'new'}
          camera={editing}
          existingIds={list.map((camera) => camera.id)}
          destinations={destinationList}
          onCancel={() => open({ kind: 'list' })}
          onSaved={(saved) => {
            setView({ kind: 'list' });
            setActionError(null);
            setNotice(editing ? `${saved.name} was saved.` : `${saved.name} was added.`);
            void cameras.reload();
          }}
        />
      </>
    );
  }

  if (isAdmin && view.kind === 'import') {
    return (
      <>
        <div className="page-header">
          <h1>Cameras</h1>
        </div>
        <CameraImport onImported={() => void cameras.reload()} onClose={() => open({ kind: 'list' })} />
      </>
    );
  }

  return (
    <>
      <div className="page-header">
        <h1>Cameras</h1>
        {isAdmin ? (
          <div className="button-row">
            <button type="button" className="button button-primary" onClick={() => open({ kind: 'add' })}>
              Add camera
            </button>
            <button type="button" className="button" onClick={() => open({ kind: 'import' })}>
              Bulk import
            </button>
          </div>
        ) : null}
      </div>

      {notice ? <Alert kind="success">{notice}</Alert> : null}
      {actionError ? <Alert kind="error">{actionError}</Alert> : null}
      {cameras.error ? <Alert kind="error">Cameras could not be loaded: {cameras.error}</Alert> : null}

      <section className="panel">
        {cameras.data === null ? (
          cameras.loading ? (
            <p className="muted">Loading…</p>
          ) : null
        ) : list.length === 0 ? (
          <p>No cameras have been added yet.</p>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">State</th>
                  <th scope="col">Address</th>
                  <th scope="col">Transport</th>
                  <th scope="col">Audio</th>
                  <th scope="col">Destinations</th>
                  {isAdmin ? <th scope="col">Actions</th> : null}
                </tr>
              </thead>
              <tbody>
                {list.map((camera) => (
                  <tr key={camera.id}>
                    <th scope="row">
                      {camera.name}
                      <div className="cell-note mono">{camera.id}</div>
                    </th>
                    <td>
                      <CameraStateBadge state={camera.stats?.state ?? (camera.enabled ? null : 'disabled')} />
                      {camera.stats && camera.stats.message !== '' ? (
                        <div className="cell-note">{camera.stats.message}</div>
                      ) : null}
                    </td>
                    <td className="mono wrap-anywhere cell-path">
                      rtsp://{camera.host}:{camera.port}
                      {camera.path}
                    </td>
                    <td>{camera.transport.toUpperCase()}</td>
                    <td>{camera.recordAudio ? 'Recorded' : 'Off'}</td>
                    <td>{destinationNames(camera)}</td>
                    {isAdmin ? (
                      <td>
                        <div className="button-row">
                          <button
                            type="button"
                            className="button button-small"
                            onClick={() => open({ kind: 'edit', camera })}
                            aria-label={`Edit ${camera.name}`}
                          >
                            Edit
                          </button>
                          <button
                            type="button"
                            className="button button-small"
                            disabled={busyId === camera.id}
                            onClick={() => void toggleEnabled(camera)}
                            aria-label={`${camera.enabled ? 'Disable' : 'Enable'} ${camera.name}`}
                          >
                            {camera.enabled ? 'Disable' : 'Enable'}
                          </button>
                          <button
                            type="button"
                            className="button button-small button-danger"
                            onClick={() => askDelete(camera)}
                            aria-label={`Delete ${camera.name}`}
                          >
                            Delete
                          </button>
                        </div>
                      </td>
                    ) : null}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          confirmLabel={keepRecordings ? 'Delete camera' : 'Delete camera and recordings'}
          danger
          busy={deleteBusy}
          error={deleteError}
          onConfirm={() => void confirmDelete()}
          onCancel={() => setDeleting(null)}
        >
          <p>The camera stops recording and is removed from the settings.</p>
          <CheckboxField
            label="Keep recordings"
            checked={keepRecordings}
            onChange={setKeepRecordings}
            disabled={deleteBusy}
            hint={
              keepRecordings
                ? 'Files already recorded stay where they are and remain listed on the Recordings page.'
                : "Every recording of this camera will be deleted from staging and all destinations. This can't be undone."
            }
          />
        </ConfirmDialog>
      ) : null}
    </>
  );
}
