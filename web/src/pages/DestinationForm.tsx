import { useState } from 'react';
import type { FormEvent } from 'react';
import { api } from '../api/client';
import type { Destination, DestinationInput, DestinationTestResult, DestinationType } from '../api/types';
import { Alert } from '../components/Alert';
import { CheckboxField, SelectField, TextField } from '../components/Fields';
import { formatBytes } from '../lib/format';
import type { FieldErrors } from '../lib/forms';
import { hasErrors, serverErrors } from '../lib/forms';
import { looksLikeDrivePath, looksLikeUncPath, parseWholeNumber } from '../lib/validate';

interface Values {
  name: string;
  type: DestinationType;
  path: string;
  username: string;
  password: string;
  clearPassword: boolean;
  capGB: string;
  maxAgeDays: string;
  enabled: boolean;
}

/** Server field names (DestinationInput) that have an input on this form. */
const FIELDS = [
  'name',
  'type',
  'path',
  'username',
  'password',
  'clearPassword',
  'capGB',
  'maxAgeDays',
  'enabled',
] as const;

function initialValues(destination: Destination | null): Values {
  if (destination === null) {
    return {
      name: '',
      type: 'local',
      path: '',
      username: '',
      password: '',
      clearPassword: false,
      capGB: '',
      maxAgeDays: '',
      enabled: true,
    };
  }
  return {
    name: destination.name,
    type: destination.type === 'unc' ? 'unc' : 'local',
    path: destination.path,
    username: destination.username ?? '',
    password: '',
    clearPassword: false,
    capGB: String(destination.capGB),
    maxAgeDays: destination.maxAgeDays === null || destination.maxAgeDays === undefined ? '' : String(destination.maxAgeDays),
    enabled: destination.enabled,
  };
}

/** Checks the fields a test needs: type and path. */
function checkPath(values: Values): FieldErrors {
  const errors: FieldErrors = {};
  const path = values.path.trim();
  if (values.type === 'local') {
    // Mapped drive letters are detected by the server, which knows the drive types.
    if (path === '') {
      errors.path = 'Enter the folder on a local or attached drive, for example E:\\Recordings.';
    } else if (path.startsWith('\\\\') || path.startsWith('//')) {
      errors.path = 'That is a network share path. Change the type to "Network share (UNC)" to use it.';
    } else if (!looksLikeDrivePath(path)) {
      errors.path = 'Enter the full path starting with a drive letter, for example E:\\Recordings.';
    }
  } else if (path === '') {
    errors.path = 'Enter the share path, for example \\\\nas01\\cameras.';
  } else if (!looksLikeUncPath(path)) {
    errors.path = 'A share path looks like \\\\server\\share, optionally followed by a folder.';
  }
  return errors;
}

function checkAll(values: Values): FieldErrors {
  const errors = checkPath(values);
  if (values.name.trim() === '') errors.name = 'Enter a name for the destination.';

  const cap = parseWholeNumber(values.capGB);
  if (values.capGB.trim() === '') {
    errors.capGB = 'Enter the size limit in GB. The oldest recordings are deleted when it is reached.';
  } else if (cap === null || cap < 1) {
    errors.capGB = 'Size limit must be a whole number of GB, 1 or more.';
  }

  if (values.maxAgeDays.trim() !== '') {
    const days = parseWholeNumber(values.maxAgeDays);
    if (days === null || days < 1) {
      errors.maxAgeDays = 'Maximum age must be a whole number of days, 1 or more. Leave it blank for no age limit.';
    }
  }
  return errors;
}

function toInput(values: Values, destination: Destination | null): DestinationInput {
  const unc = values.type === 'unc';
  const input: DestinationInput = {
    name: values.name.trim(),
    type: values.type,
    path: values.path.trim(),
    username: unc ? values.username.trim() : '',
    capGB: parseWholeNumber(values.capGB) ?? 0,
    maxAgeDays: values.maxAgeDays.trim() === '' ? null : parseWholeNumber(values.maxAgeDays),
    enabled: values.enabled,
  };
  if (unc && values.password !== '') input.password = values.password;
  // A local folder needs no credentials, so a saved share password is dropped.
  if (destination !== null && destination.hasPassword && (values.clearPassword || !unc)) {
    input.clearPassword = true;
  }
  return input;
}

function TestResult({ result }: { result: DestinationTestResult }) {
  if (!result.ok) {
    return (
      <Alert kind="error">
        The test failed:
        <pre className="error-text">{result.error || 'No error text was returned.'}</pre>
      </Alert>
    );
  }
  return (
    <Alert kind="success">
      The test file was written, read back and deleted in {result.durationMs} ms. Free space:{' '}
      {formatBytes(result.freeBytes)} of {formatBytes(result.totalBytes)}.
    </Alert>
  );
}

interface DestinationFormProps {
  /** The destination being edited, or `null` to add a new one. */
  destination: Destination | null;
  onSaved: (destination: Destination) => void;
  onCancel: () => void;
}

export function DestinationForm({ destination, onSaved, onCancel }: DestinationFormProps) {
  const [values, setValues] = useState<Values>(() => initialValues(destination));
  const [errors, setErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<DestinationTestResult | null>(null);
  const [testError, setTestError] = useState<string | null>(null);
  const busy = saving || testing;
  const unc = values.type === 'unc';

  function set<K extends keyof Values>(key: K, value: Values[K]) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    setFormError(null);
    const found = checkAll(values);
    setErrors(found);
    if (hasErrors(found)) {
      setFormError('Fix the highlighted fields.');
      return;
    }

    setSaving(true);
    try {
      const input = toInput(values, destination);
      const saved =
        destination === null
          ? await api.createDestination(input)
          : await api.updateDestination(destination.id, input);
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
    const found = checkPath(values);
    setErrors(found);
    if (hasErrors(found)) {
      setTestError('Fix the path, then test again.');
      return;
    }

    setTesting(true);
    try {
      const input = toInput(values, destination);
      // On an existing destination the id lets the server reuse the saved password.
      if (destination !== null) input.id = destination.id;
      setTestResult(await api.testDestination(input));
    } catch (err) {
      const failure = serverErrors(err, FIELDS);
      setErrors(failure.fields);
      setTestError(`The test could not be run: ${failure.message}`);
    } finally {
      setTesting(false);
    }
  }

  let passwordHint = 'Leave the user name and password blank to connect as the ButterCams service account.';
  if (destination !== null && destination.hasPassword) {
    passwordHint = 'A password is saved. Leave this blank to keep it.';
  }

  return (
    <form className="panel form" onSubmit={(event) => void save(event)} noValidate>
      <h2>{destination === null ? 'Add destination' : `Edit ${destination.name}`}</h2>
      {formError ? <Alert kind="error">{formError}</Alert> : null}

      <TextField
        label="Name"
        value={values.name}
        onChange={(name) => set('name', name)}
        error={errors.name}
        autoFocus
      />

      <div className="form-grid form-grid-path">
        <SelectField
          label="Type"
          value={values.type}
          onChange={(type) => set('type', type === 'unc' ? 'unc' : 'local')}
          error={errors.type}
        >
          <option value="local">Local or attached drive</option>
          <option value="unc">Network share (UNC)</option>
        </SelectField>
        <TextField
          label="Path"
          value={values.path}
          onChange={(path) => set('path', path)}
          error={errors.path}
          spellCheck={false}
          autoCapitalize="none"
          placeholder={unc ? '\\\\nas01\\cameras' : 'E:\\Recordings'}
        />
      </div>

      {unc ? (
        <div className="form-grid">
          <TextField
            label="User name"
            value={values.username}
            onChange={(username) => set('username', username)}
            error={errors.username}
            spellCheck={false}
            autoCapitalize="none"
            autoComplete="off"
            placeholder="CORP\svc-buttercams"
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
            {destination !== null && destination.hasPassword ? (
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
      ) : null}

      <div className="form-grid">
        <TextField
          label="Size limit (GB)"
          value={values.capGB}
          onChange={(capGB) => set('capGB', capGB)}
          error={errors.capGB}
          inputMode="numeric"
          hint="When ButterCams' recordings here reach this size, the oldest are deleted first."
        />
        <TextField
          label="Maximum age (days)"
          value={values.maxAgeDays}
          onChange={(maxAgeDays) => set('maxAgeDays', maxAgeDays)}
          error={errors.maxAgeDays}
          inputMode="numeric"
          hint="Recordings older than this are deleted. Leave blank for no age limit."
        />
      </div>

      <CheckboxField
        label="Enabled (copy recordings to this destination)"
        checked={values.enabled}
        onChange={(enabled) => set('enabled', enabled)}
        error={errors.enabled}
      />

      <div className="button-row">
        <button type="submit" className="button button-primary" disabled={busy}>
          {saving ? 'Saving…' : destination === null ? 'Add destination' : 'Save changes'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={() => void test()}>
          {testing ? 'Testing…' : 'Test'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>

      {testing ? (
        <p className="muted" role="status">
          Writing a 1 MB test file to the destination and reading it back.
        </p>
      ) : null}
      {testError ? <Alert kind="error">{testError}</Alert> : null}
      {testResult ? <TestResult result={testResult} /> : null}
    </form>
  );
}
