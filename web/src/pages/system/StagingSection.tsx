import { useState } from 'react';
import type { FormEvent } from 'react';
import { api } from '../../api/client';
import type { SystemInfo } from '../../api/types';
import { Alert } from '../../components/Alert';
import { TextField } from '../../components/Fields';
import { formatBytes } from '../../lib/format';
import type { FieldErrors } from '../../lib/forms';
import { hasErrors, serverErrors } from '../../lib/forms';
import { checkWholeNumber, looksLikeDrivePath } from '../../lib/validate';

const FIELDS = ['path', 'minFreePercent', 'minFreeGB'] as const;

interface Values {
  path: string;
  minFreePercent: string;
  minFreeGB: string;
}

function fromSystem(system: SystemInfo): Values {
  return {
    path: system.staging.path,
    minFreePercent: String(system.staging.minFreePercent),
    minFreeGB: String(system.staging.minFreeGB),
  };
}

function check(values: Values): FieldErrors {
  const errors: FieldErrors = {};
  const path = values.path.trim();
  if (path === '') {
    errors.path = 'Enter the staging folder, for example D:\\ButterCams\\staging.';
  } else if (path.startsWith('\\\\') || path.startsWith('//')) {
    errors.path = 'Staging must be on a local disk, not a network share.';
  } else if (!looksLikeDrivePath(path)) {
    errors.path = 'Enter the full path starting with a drive letter, for example D:\\ButterCams\\staging.';
  }
  const percentError = checkWholeNumber(
    values.minFreePercent,
    0,
    100,
    'Minimum free space must be a whole percentage between 0 and 100.',
  );
  if (percentError) errors.minFreePercent = percentError;
  const gbError = checkWholeNumber(
    values.minFreeGB,
    0,
    Number.MAX_SAFE_INTEGER,
    'Minimum free space must be a whole number of GB, 0 or more.',
  );
  if (gbError) errors.minFreeGB = gbError;
  return errors;
}

interface SectionProps {
  system: SystemInfo;
  onUpdated: (system: SystemInfo) => void;
}

export function StagingSection({ system, onUpdated }: SectionProps) {
  const [values, setValues] = useState<Values>(() => fromSystem(system));
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  /** After a save: whether the folder itself changed. */
  const [saved, setSaved] = useState<false | 'folder' | 'floor'>(false);
  const [busy, setBusy] = useState(false);

  function set<K extends keyof Values>(key: K, value: Values[K]) {
    setValues((current) => ({ ...current, [key]: value }));
    setSaved(false);
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    setSaved(false);
    const found = check(values);
    setErrors(found);
    if (hasErrors(found)) return;

    setBusy(true);
    try {
      const updated = await api.updateSystem({
        staging: {
          path: values.path.trim(),
          minFreePercent: Number(values.minFreePercent.trim()),
          minFreeGB: Number(values.minFreeGB.trim()),
        },
      });
      const folderChanged = updated.staging.path !== system.staging.path;
      onUpdated(updated);
      setValues(fromSystem(updated));
      setSaved(folderChanged ? 'folder' : 'floor');
    } catch (err) {
      const result = serverErrors(err, FIELDS);
      setErrors(result.fields);
      setFailure(result.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="staging-settings-heading">
      <h2 id="staging-settings-heading">Staging</h2>
      <p className="muted">
        Cameras record into this local folder first. Currently {formatBytes(system.staging.freeBytes)} free of{' '}
        {formatBytes(system.staging.totalBytes)}.
      </p>
      <form className="form" onSubmit={(event) => void save(event)} noValidate>
        {failure ? <Alert kind="error">{failure}</Alert> : null}
        {saved ? (
          <Alert kind="success">
            {saved === 'folder'
              ? 'Staging settings saved. A new folder is used by each camera from its next hour file; files already in the old folder stay there until they are copied.'
              : 'Staging settings saved.'}
          </Alert>
        ) : null}
        <TextField
          label="Staging folder"
          value={values.path}
          onChange={(path) => set('path', path)}
          error={errors.path}
          spellCheck={false}
          autoCapitalize="none"
        />
        <div className="form-grid">
          <TextField
            label="Minimum free space (%)"
            value={values.minFreePercent}
            onChange={(minFreePercent) => set('minFreePercent', minFreePercent)}
            error={errors.minFreePercent}
            inputMode="numeric"
          />
          <TextField
            label="Minimum free space (GB)"
            value={values.minFreeGB}
            onChange={(minFreeGB) => set('minFreeGB', minFreeGB)}
            error={errors.minFreeGB}
            inputMode="numeric"
          />
        </div>
        <p className="field-hint">
          The floor is whichever of the two leaves more space free. Below it, the oldest staging files are deleted so
          that recording never stops.
        </p>
        <div className="button-row">
          <button type="submit" className="button button-primary" disabled={busy}>
            {busy ? 'Saving…' : 'Save staging settings'}
          </button>
        </div>
      </form>
    </section>
  );
}
