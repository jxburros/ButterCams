import { useState } from 'react';
import type { ClipboardEvent, FormEvent } from 'react';
import { api } from '../api/client';
import type { Camera, CameraInput, CameraTestResult, Destination, Transport } from '../api/types';
import { Alert } from '../components/Alert';
import { CheckboxField, SelectField, TextField } from '../components/Fields';
import {
  formatCodec,
  formatFps,
  formatKeyframeInterval,
  formatResolution,
} from '../lib/format';
import type { FieldErrors } from '../lib/forms';
import { hasErrors, serverErrors } from '../lib/forms';
import { DEFAULT_RTSP_PORT, parseRtspUrl } from '../lib/rtspUrl';
import { checkCameraHost, checkId, checkPort, idFromName } from '../lib/validate';

interface Values {
  name: string;
  id: string;
  host: string;
  port: string;
  path: string;
  username: string;
  password: string;
  clearPassword: boolean;
  transport: Transport;
  recordAudio: boolean;
  enabled: boolean;
  destinationIds: string[];
}

/** Server field names (CameraInput) that have an input on this form. */
const FIELDS = [
  'id',
  'name',
  'enabled',
  'url',
  'host',
  'port',
  'path',
  'username',
  'password',
  'clearPassword',
  'transport',
  'recordAudio',
  'destinationIds',
] as const;

function initialValues(camera: Camera | null): Values {
  if (camera === null) {
    return {
      name: '',
      id: '',
      host: '',
      port: String(DEFAULT_RTSP_PORT),
      path: '',
      username: '',
      password: '',
      clearPassword: false,
      transport: 'tcp',
      recordAudio: true,
      enabled: true,
      destinationIds: [],
    };
  }
  return {
    name: camera.name,
    id: camera.id,
    host: camera.host,
    port: String(camera.port),
    path: camera.path,
    username: camera.username ?? '',
    password: '',
    clearPassword: false,
    transport: camera.transport === 'udp' ? 'udp' : 'tcp',
    recordAudio: camera.recordAudio,
    enabled: camera.enabled,
    destinationIds: camera.destinationIds ?? [],
  };
}

/** Checks the fields a connection needs: host, port and path. */
function checkConnection(values: Values): FieldErrors {
  const errors: FieldErrors = {};
  const hostError = checkCameraHost(values.host);
  if (hostError) errors.host = hostError;
  const portError = checkPort(values.port);
  if (portError) errors.port = portError;
  const path = values.path.trim();
  if (/\s/.test(path)) {
    errors.path = 'Path must not contain spaces.';
  } else if (path !== '' && !path.startsWith('/')) {
    errors.path = 'Path must start with a slash, for example /stream1.';
  }
  return errors;
}

function checkAll(values: Values, creating: boolean, existingIds: readonly string[]): FieldErrors {
  const errors = checkConnection(values);
  if (values.name.trim() === '') errors.name = 'Enter a name for the camera.';
  if (creating) {
    if (values.id === '') {
      errors.id = 'Enter an ID. One is filled in for you when the name contains letters or digits.';
    } else {
      const idError = checkId(values.id);
      if (idError) errors.id = idError;
      else if (existingIds.includes(values.id)) errors.id = 'Another camera already uses this ID. Choose a different one.';
    }
  }
  return errors;
}

function toInput(values: Values, camera: Camera | null): CameraInput {
  const input: CameraInput = {
    name: values.name.trim(),
    enabled: values.enabled,
    host: values.host.trim(),
    port: Number(values.port.trim()),
    path: values.path.trim(),
    username: values.username.trim(),
    transport: values.transport,
    recordAudio: values.recordAudio,
    destinationIds: values.destinationIds,
  };
  if (camera === null) input.id = values.id;
  if (values.password !== '') input.password = values.password;
  if (camera !== null && values.clearPassword) input.clearPassword = true;
  return input;
}

function secondSessionText(allowed: boolean | null): string {
  if (allowed === null) return 'Could not be determined';
  return allowed ? 'Allowed' : 'Not allowed (the camera accepts one connection at a time)';
}

function TestResult({ result }: { result: CameraTestResult }) {
  const warnings = result.warnings ?? [];
  return (
    <div className="test-result">
      {result.ok ? (
        <div role="status">
          <Alert kind="success">The camera answered.</Alert>
          <dl className="facts">
            <div>
              <dt>Video codec</dt>
              <dd>{formatCodec(result.videoCodec)}</dd>
            </div>
            <div>
              <dt>Resolution</dt>
              <dd>{formatResolution(result.width, result.height)}</dd>
            </div>
            <div>
              <dt>Frame rate</dt>
              <dd>{formatFps(result.fps)}</dd>
            </div>
            <div>
              <dt>Keyframe interval</dt>
              <dd>{formatKeyframeInterval(result.keyframeIntervalMs)}</dd>
            </div>
            <div>
              <dt>Audio codec</dt>
              <dd>
                {result.audioCodec ? formatCodec(result.audioCodec) : 'No audio'}
                {result.audioNote ? ` (${result.audioNote})` : ''}
              </dd>
            </div>
            <div>
              <dt>Second session</dt>
              <dd>{secondSessionText(result.secondSessionAllowed)}</dd>
            </div>
          </dl>
        </div>
      ) : (
        <Alert kind="error">Connection test failed: {result.error || 'the camera did not answer.'}</Alert>
      )}
      {warnings.length > 0 ? (
        <Alert kind="warning">
          <ul className="plain-list">
            {warnings.map((warning) => (
              <li key={warning}>{warning}</li>
            ))}
          </ul>
        </Alert>
      ) : null}
    </div>
  );
}

interface CameraFormProps {
  /** The camera being edited, or `null` to add a new one. */
  camera: Camera | null;
  /** IDs already taken by other cameras. */
  existingIds: readonly string[];
  destinations: readonly Destination[];
  onSaved: (camera: Camera) => void;
  onCancel: () => void;
}

export function CameraForm({ camera, existingIds, destinations, onSaved, onCancel }: CameraFormProps) {
  const creating = camera === null;
  const [values, setValues] = useState<Values>(() => initialValues(camera));
  const [idEdited, setIdEdited] = useState(false);
  const [url, setUrl] = useState('');
  const [urlNote, setUrlNote] = useState<string | null>(null);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<CameraTestResult | null>(null);
  const [testError, setTestError] = useState<string | null>(null);
  const busy = saving || testing;

  function set<K extends keyof Values>(key: K, value: Values[K]) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function setName(name: string) {
    setValues((current) => ({
      ...current,
      name,
      id: creating && !idEdited ? idFromName(name) : current.id,
    }));
  }

  /**
   * Splits a full rtsp:// URL into host, port, path and (when the URL carries
   * them) user name and password. Returns the new values, or `null` when the
   * text is not a usable URL.
   */
  function splitUrl(text: string): Values | null {
    const result = parseRtspUrl(text);
    if (!result.ok) {
      setUrlNote(null);
      setErrors((current) => ({ ...current, url: result.message }));
      return null;
    }
    const { host, port, path, username, password } = result.parts;
    const next: Values = { ...values, host, port: String(port), path };
    if (username !== null) {
      next.username = username;
      next.password = password ?? '';
      next.clearPassword = false;
    }
    setValues(next);
    setUrl('');
    setUrlNote(
      username !== null
        ? 'Host, port, path, user name and password were filled in from the URL.'
        : 'Host, port and path were filled in from the URL.',
    );
    setErrors((current) => {
      const { url: _url, host: _host, port: _port, path: _path, ...rest } = current;
      return rest;
    });
    return next;
  }

  function pasteUrl(event: ClipboardEvent<HTMLInputElement>) {
    const text = event.clipboardData.getData('text');
    if (parseRtspUrl(text).ok) {
      event.preventDefault();
      splitUrl(text);
    }
  }

  /** The values to send, after splitting a URL still sitting in the URL box. */
  function currentValues(): Values | null {
    return url.trim() === '' ? values : splitUrl(url);
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    setFormError(null);
    const current = currentValues();
    if (current === null) {
      setFormError('Fix the highlighted fields.');
      return;
    }
    const found = checkAll(current, creating, existingIds);
    setErrors(found);
    if (hasErrors(found)) {
      setFormError('Fix the highlighted fields.');
      return;
    }

    setSaving(true);
    try {
      const input = toInput(current, camera);
      const saved = camera === null ? await api.createCamera(input) : await api.updateCamera(camera.id, input);
      onSaved(saved);
    } catch (err) {
      const failure = serverErrors(err, FIELDS);
      setErrors(failure.fields);
      setFormError(failure.message);
      setSaving(false);
    }
  }

  async function test() {
    setFormError(null);
    setTestError(null);
    setTestResult(null);
    const current = currentValues();
    if (current === null) {
      setTestError('Correct the RTSP URL or clear that box, then test again.');
      return;
    }
    const found = checkConnection(current);
    setErrors(found);
    if (hasErrors(found)) {
      setTestError('Fix the highlighted connection fields, then test again.');
      return;
    }

    setTesting(true);
    try {
      const input = toInput(current, camera);
      // On an existing camera the id lets the server reuse the saved password.
      if (camera === null) delete input.id;
      else input.id = camera.id;
      setTestResult(await api.testCamera(input));
    } catch (err) {
      const failure = serverErrors(err, FIELDS);
      setErrors(failure.fields);
      setTestError(`The test could not be run: ${failure.message}`);
    } finally {
      setTesting(false);
    }
  }

  function toggleDestination(id: string, checked: boolean) {
    setValues((current) => ({
      ...current,
      destinationIds: checked
        ? [...current.destinationIds.filter((item) => item !== id), id]
        : current.destinationIds.filter((item) => item !== id),
    }));
  }

  let passwordHint: string | undefined;
  if (camera !== null) {
    passwordHint = camera.hasPassword
      ? 'A password is saved. Leave this blank to keep it.'
      : 'No password is saved. Leave this blank if the camera needs none.';
  }

  return (
    <form className="panel form" onSubmit={(event) => void save(event)} noValidate>
      <h2>{camera === null ? 'Add camera' : `Edit ${camera.name}`}</h2>
      {formError ? <Alert kind="error">{formError}</Alert> : null}

      <div className="form-grid">
        <TextField
          label="Name"
          value={values.name}
          onChange={setName}
          error={errors.name}
          autoFocus
        />
        {camera === null ? (
          <TextField
            label="ID"
            value={values.id}
            onChange={(id) => {
              setIdEdited(true);
              set('id', id);
            }}
            error={errors.id}
            maxLength={48}
            spellCheck={false}
            autoCapitalize="none"
            hint="Names this camera's folders and files. Lowercase letters, digits and hyphens. It cannot be changed later."
          />
        ) : (
          <TextField
            label="ID"
            value={camera.id}
            onChange={() => undefined}
            readOnly
            hint="The ID cannot be changed after the camera is created."
          />
        )}
      </div>

      <fieldset>
        <legend>Connection</legend>
        <TextField
          label="RTSP URL (optional)"
          value={url}
          onChange={(text) => {
            setUrl(text);
            setUrlNote(null);
          }}
          onPaste={pasteUrl}
          onBlur={() => {
            if (url.trim() !== '') splitUrl(url);
          }}
          error={errors.url}
          spellCheck={false}
          autoCapitalize="none"
          autoComplete="off"
          placeholder="rtsp://user:password@192.168.1.50:554/stream1"
          hint={
            urlNote ??
            'Paste the full URL to fill in the fields below. A user name and password inside the URL are moved to their own fields.'
          }
        />
        <div className="form-grid form-grid-3">
          <TextField
            label="Host"
            value={values.host}
            onChange={(host) => set('host', host)}
            error={errors.host}
            spellCheck={false}
            autoCapitalize="none"
            placeholder="192.168.1.50"
          />
          <TextField
            label="Port"
            value={values.port}
            onChange={(port) => set('port', port)}
            error={errors.port}
            inputMode="numeric"
            maxLength={5}
          />
          <TextField
            label="Path"
            value={values.path}
            onChange={(path) => set('path', path)}
            error={errors.path}
            spellCheck={false}
            autoCapitalize="none"
            placeholder="/stream1"
          />
        </div>
        <div className="form-grid">
          <TextField
            label="User name"
            value={values.username}
            onChange={(username) => set('username', username)}
            error={errors.username}
            spellCheck={false}
            autoCapitalize="none"
            autoComplete="off"
          />
          <div>
            <TextField
              label="Password"
              type="password"
              value={values.password}
              onChange={(password) => set('password', password)}
              error={errors.password}
              autoComplete="new-password"
              disabled={values.clearPassword}
              hint={passwordHint}
            />
            {camera !== null && camera.hasPassword ? (
              <CheckboxField
                label="Remove the saved password"
                checked={values.clearPassword}
                onChange={(checked) =>
                  setValues((current) => ({
                    ...current,
                    clearPassword: checked,
                    password: checked ? '' : current.password,
                  }))
                }
                error={errors.clearPassword}
              />
            ) : null}
          </div>
        </div>
        <SelectField
          label="Transport"
          value={values.transport}
          onChange={(transport) => set('transport', transport === 'udp' ? 'udp' : 'tcp')}
          error={errors.transport}
        >
          <option value="tcp">TCP (default)</option>
          <option value="udp">UDP</option>
        </SelectField>
      </fieldset>

      <fieldset>
        <legend>Recording</legend>
        <CheckboxField
          label="Enabled (record this camera)"
          checked={values.enabled}
          onChange={(enabled) => set('enabled', enabled)}
          error={errors.enabled}
        />
        <CheckboxField
          label="Record audio when the camera sends it"
          checked={values.recordAudio}
          onChange={(recordAudio) => set('recordAudio', recordAudio)}
          error={errors.recordAudio}
        />
      </fieldset>

      <fieldset aria-describedby={errors.destinationIds ? 'camera-destinations-error' : undefined}>
        <legend>Destinations</legend>
        {destinations.length === 0 ? (
          <p className="muted">
            No destinations have been added yet. Until one is chosen, this camera's recordings stay in staging.
          </p>
        ) : (
          <>
            <p className="field-hint">
              Each closed hour file is copied to every destination ticked here. With none ticked, recordings stay in
              staging.
            </p>
            {destinations.map((destination) => (
              <CheckboxField
                key={destination.id}
                label={destination.enabled ? destination.name : `${destination.name} (disabled)`}
                checked={values.destinationIds.includes(destination.id)}
                onChange={(checked) => toggleDestination(destination.id, checked)}
              />
            ))}
          </>
        )}
        {errors.destinationIds ? (
          <p id="camera-destinations-error" className="field-error">
            {errors.destinationIds}
          </p>
        ) : null}
      </fieldset>

      <div className="button-row">
        <button type="submit" className="button button-primary" disabled={busy}>
          {saving ? 'Saving…' : camera === null ? 'Add camera' : 'Save changes'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={() => void test()}>
          {testing ? 'Testing…' : 'Test connection'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>

      {testing ? (
        <p className="muted" role="status">
          Connecting to the camera. This can take up to 30 seconds.
        </p>
      ) : null}
      {testError ? <Alert kind="error">{testError}</Alert> : null}
      {testResult ? <TestResult result={testResult} /> : null}
    </form>
  );
}

export function cameraToInput(camera: Camera): CameraInput {
  return {
    name: camera.name,
    enabled: camera.enabled,
    host: camera.host,
    port: camera.port,
    path: camera.path,
    username: camera.username,
    transport: camera.transport,
    recordAudio: camera.recordAudio,
    destinationIds: camera.destinationIds,
  };
}
