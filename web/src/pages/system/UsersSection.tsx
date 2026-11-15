import { useId, useState } from 'react';
import type { FormEvent } from 'react';
import { api, errorMessage } from '../../api/client';
import type { Camera, Role, UserAccount, UserUpdate } from '../../api/types';
import { useAuth } from '../../auth/AuthContext';
import { Alert } from '../../components/Alert';
import { Badge } from '../../components/Badge';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { SelectField, TextField } from '../../components/Fields';
import { formatDate } from '../../lib/format';
import type { FieldErrors } from '../../lib/forms';
import { hasErrors, serverErrors } from '../../lib/forms';
import { useLoad } from '../../lib/hooks';
import { checkPassword } from '../../lib/validate';
import '../../live.css';

const FIELDS = ['username', 'password', 'role', 'cameras'] as const;

const ROLE_LABELS: Record<Role, string> = {
  admin: 'Admin',
  viewer: 'Viewer',
};

function toRole(value: string): Role {
  return value === 'admin' ? 'admin' : 'viewer';
}

function RoleOptions() {
  return (
    <>
      <option value="viewer">Viewer (live view, recordings and status)</option>
      <option value="admin">Admin (everything)</option>
    </>
  );
}

interface FormProps {
  cameras: Camera[];
  onDone: (message: string) => void;
  onCancel: () => void;
}

/** `null` = every camera; a list = only those (spec 16.6). */
type Limit = string[] | null;

function sameLimit(a: Limit, b: Limit): boolean {
  if (a === null || b === null) return a === b;
  return a.length === b.length && [...a].sort().join('\n') === [...b].sort().join('\n');
}

function limitText(account: UserAccount, cameras: Camera[]): string {
  if (account.role === 'admin' || account.cameras === null) return 'All cameras';
  if (account.cameras.length === 0) return 'No cameras';
  const names = account.cameras.map((id) => cameras.find((camera) => camera.id === id)?.name ?? id);
  return names.length <= 3 ? names.join(', ') : `${names.slice(0, 3).join(', ')} and ${names.length - 3} more`;
}

/** Which cameras a Viewer may see. Admins always see all of them. */
function CameraLimit({
  role,
  cameras,
  value,
  onChange,
  error,
}: {
  role: Role;
  cameras: Camera[];
  value: Limit;
  onChange: (value: Limit) => void;
  error?: string;
}) {
  const name = useId();
  if (role === 'admin') {
    return <p className="field-hint">Admins always see every camera.</p>;
  }
  // Cameras in the limit that were deleted since stay listed, so saving does not silently drop them.
  const known = new Set(cameras.map((camera) => camera.id));
  const gone = (value ?? []).filter((id) => !known.has(id));
  return (
    <fieldset className="camera-limit">
      <legend>Cameras this user may see</legend>
      <div className="check-row">
        <input id={`${name}-all`} type="radio" name={name} checked={value === null} onChange={() => onChange(null)} />
        <label htmlFor={`${name}-all`}>All cameras, including ones added later</label>
      </div>
      <div className="check-row">
        <input id={`${name}-some`} type="radio" name={name} checked={value !== null} onChange={() => onChange(value ?? [])} />
        <label htmlFor={`${name}-some`}>Only the cameras ticked below</label>
      </div>
      {value !== null ? (
        cameras.length + gone.length === 0 ? (
          <p className="muted">No cameras have been added yet.</p>
        ) : (
          <div className="pick-grid">
            {[...cameras.map((camera) => ({ id: camera.id, name: camera.name })), ...gone.map((id) => ({ id, name: `${id} (deleted)` }))].map(
              (camera) => {
                const on = value.includes(camera.id);
                return (
                  <label key={camera.id} className={on ? 'pick pick-on' : 'pick'}>
                    <input
                      type="checkbox"
                      checked={on}
                      onChange={() => onChange(on ? value.filter((id) => id !== camera.id) : [...value, camera.id])}
                    />
                    <span className="pick-name">{camera.name}</span>
                  </label>
                );
              },
            )}
          </div>
        )
      ) : null}
      <p className="field-hint">
        The limit applies to live view, recordings, downloads and events. A user limited to no cameras sees nothing.
      </p>
      {error ? <p className="field-error">{error}</p> : null}
    </fieldset>
  );
}

function AddUserForm({ cameras, onDone, onCancel }: FormProps) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<Role>('viewer');
  const [limit, setLimit] = useState<Limit>(null);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    const found: FieldErrors = {};
    const name = username.trim();
    if (name === '') found.username = 'Enter a user name.';
    const passwordError = checkPassword(password);
    if (passwordError) found.password = passwordError;
    setErrors(found);
    if (hasErrors(found)) return;

    setBusy(true);
    try {
      await api.createUser({ username: name, password, role, cameras: role === 'viewer' ? limit : null });
      onDone(`User ${name} was added.`);
    } catch (err) {
      const result = serverErrors(err, FIELDS);
      setErrors(result.fields);
      setFailure(result.message);
      setBusy(false);
    }
  }

  return (
    <form className="form subform" onSubmit={(event) => void submit(event)} noValidate>
      <h3>Add user</h3>
      {failure ? <Alert kind="error">{failure}</Alert> : null}
      <div className="form-grid form-grid-3">
        <TextField
          label="User name"
          value={username}
          onChange={setUsername}
          error={errors.username}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          autoFocus
        />
        <TextField
          label="Password"
          type="password"
          value={password}
          onChange={setPassword}
          error={errors.password}
          autoComplete="new-password"
          hint="At least 8 characters."
        />
        <SelectField label="Role" value={role} onChange={(value) => setRole(toRole(value))} error={errors.role}>
          <RoleOptions />
        </SelectField>
      </div>
      <CameraLimit role={role} cameras={cameras} value={limit} onChange={setLimit} error={errors.cameras} />
      <div className="button-row">
        <button type="submit" className="button button-primary" disabled={busy}>
          {busy ? 'Adding…' : 'Add user'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function EditUserForm({ account, cameras, onDone, onCancel }: FormProps & { account: UserAccount }) {
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<Role>(account.role);
  const [limit, setLimit] = useState<Limit>(account.cameras);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    const found: FieldErrors = {};
    const update: UserUpdate = {};
    if (role !== account.role) update.role = role;
    if (password !== '') {
      const passwordError = checkPassword(password);
      if (passwordError) found.password = passwordError;
      update.password = password;
    }
    if (role === 'viewer' && !sameLimit(limit, account.cameras)) update.cameras = limit;
    setErrors(found);
    if (hasErrors(found)) return;
    if (update.role === undefined && update.password === undefined && update.cameras === undefined) {
      setFailure('Nothing to save: choose a different role or cameras, or enter a new password.');
      return;
    }

    setBusy(true);
    try {
      await api.updateUser(account.username, update);
      onDone(`User ${account.username} was updated.`);
    } catch (err) {
      const result = serverErrors(err, FIELDS);
      setErrors(result.fields);
      setFailure(result.message);
      setBusy(false);
    }
  }

  return (
    <form className="form subform" onSubmit={(event) => void submit(event)} noValidate>
      <h3>Change {account.username}</h3>
      {failure ? <Alert kind="error">{failure}</Alert> : null}
      <div className="form-grid">
        <SelectField label="Role" value={role} onChange={(value) => setRole(toRole(value))} error={errors.role}>
          <RoleOptions />
        </SelectField>
        <TextField
          label="New password"
          type="password"
          value={password}
          onChange={setPassword}
          error={errors.password}
          autoComplete="new-password"
          hint="At least 8 characters. Leave blank to keep the current password."
          autoFocus
        />
      </div>
      <CameraLimit role={role} cameras={cameras} value={limit} onChange={setLimit} error={errors.cameras} />
      <div className="button-row">
        <button type="submit" className="button button-primary" disabled={busy}>
          {busy ? 'Saving…' : 'Save changes'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

type Editing = { kind: 'none' } | { kind: 'add' } | { kind: 'edit'; account: UserAccount };

export function UsersSection() {
  const { user: me, refresh } = useAuth();
  const users = useLoad(api.users);
  const cameras = useLoad(api.cameras);
  const cameraList = cameras.data?.cameras ?? [];
  const [editing, setEditing] = useState<Editing>({ kind: 'none' });
  const [notice, setNotice] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<UserAccount | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const list = users.data?.users ?? [];

  /** After a change to the signed-in account, its role or session may differ. */
  async function afterChange(username: string, message: string) {
    setEditing({ kind: 'none' });
    setNotice(message);
    await users.reload();
    if (username === me?.username) await refresh();
  }

  async function confirmDelete() {
    if (deleting === null) return;
    setDeleteBusy(true);
    setDeleteError(null);
    try {
      await api.deleteUser(deleting.username);
      setDeleting(null);
      await afterChange(deleting.username, `User ${deleting.username} was deleted.`);
    } catch (err) {
      // Includes the server's refusal to delete the last admin.
      setDeleteError(errorMessage(err));
    } finally {
      setDeleteBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="users-heading">
      <div className="panel-header">
        <h2 id="users-heading">Users</h2>
        {editing.kind === 'none' ? (
          <button
            type="button"
            className="button"
            onClick={() => {
              setNotice(null);
              setEditing({ kind: 'add' });
            }}
          >
            Add user
          </button>
        ) : null}
      </div>

      {notice ? <Alert kind="success">{notice}</Alert> : null}
      {users.error ? <Alert kind="error">Users could not be loaded: {users.error}</Alert> : null}

      {users.data === null ? (
        users.loading ? (
          <p className="muted">Loading…</p>
        ) : null
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">User name</th>
                <th scope="col">Role</th>
                <th scope="col">Cameras</th>
                <th scope="col">Created</th>
                <th scope="col">Status</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {list.map((account) => (
                <tr key={account.username}>
                  <th scope="row">
                    {account.username}
                    {account.username === me?.username ? <span className="cell-note"> (you)</span> : null}
                  </th>
                  <td>{ROLE_LABELS[account.role] ?? account.role}</td>
                  <td>{limitText(account, cameraList)}</td>
                  <td>{formatDate(account.createdAt)}</td>
                  <td>
                    {account.locked ? <Badge tone="amber">Locked</Badge> : <Badge tone="green">Active</Badge>}
                  </td>
                  <td>
                    <div className="button-row">
                      <button
                        type="button"
                        className="button button-small"
                        onClick={() => {
                          setNotice(null);
                          setEditing({ kind: 'edit', account });
                        }}
                        aria-label={`Change password, role or cameras of ${account.username}`}
                      >
                        Change
                      </button>
                      <button
                        type="button"
                        className="button button-small button-danger"
                        onClick={() => {
                          setDeleteError(null);
                          setDeleting(account);
                        }}
                        aria-label={`Delete ${account.username}`}
                      >
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {editing.kind === 'add' ? (
        <AddUserForm
          cameras={cameraList}
          onCancel={() => setEditing({ kind: 'none' })}
          onDone={(message) => void afterChange('', message)}
        />
      ) : null}
      {editing.kind === 'edit' ? (
        <EditUserForm
          key={editing.account.username}
          account={editing.account}
          cameras={cameraList}
          onCancel={() => setEditing({ kind: 'none' })}
          onDone={(message) => void afterChange(editing.account.username, message)}
        />
      ) : null}

      {deleting ? (
        <ConfirmDialog
          title={`Delete user ${deleting.username}?`}
          confirmLabel="Delete user"
          danger
          busy={deleteBusy}
          error={deleteError}
          onConfirm={() => void confirmDelete()}
          onCancel={() => setDeleting(null)}
        >
          <p>
            {deleting.username === me?.username
              ? 'This is the account you are signed in with. You will be signed out.'
              : 'This person will no longer be able to sign in.'}
          </p>
        </ConfirmDialog>
      ) : null}
    </section>
  );
}
