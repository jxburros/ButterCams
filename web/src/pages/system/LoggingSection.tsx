import { useState } from 'react';
import type { FormEvent } from 'react';
import { api } from '../../api/client';
import type { SystemInfo } from '../../api/types';
import { Alert } from '../../components/Alert';
import { TextField } from '../../components/Fields';
import type { FieldErrors } from '../../lib/forms';
import { serverErrors } from '../../lib/forms';
import { checkWholeNumber } from '../../lib/validate';

const FIELDS = ['retentionDays'] as const;

interface SectionProps {
  system: SystemInfo;
  onUpdated: (system: SystemInfo) => void;
}

export function LoggingSection({ system, onUpdated }: SectionProps) {
  const [retentionDays, setRetentionDays] = useState(() => String(system.logging.retentionDays));
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  async function save(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    setSaved(false);
    const problem = checkWholeNumber(
      retentionDays,
      1,
      365,
      'Log retention must be a whole number of days between 1 and 365.',
    );
    setErrors(problem ? { retentionDays: problem } : {});
    if (problem) return;

    setBusy(true);
    try {
      const updated = await api.updateSystem({ logging: { retentionDays: Number(retentionDays.trim()) } });
      onUpdated(updated);
      setRetentionDays(String(updated.logging.retentionDays));
      setSaved(true);
    } catch (err) {
      const result = serverErrors(err, FIELDS);
      setErrors(result.fields);
      setFailure(result.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="logging-heading">
      <h2 id="logging-heading">Log retention</h2>
      <form className="form" onSubmit={(event) => void save(event)} noValidate>
        {failure ? <Alert kind="error">{failure}</Alert> : null}
        {saved ? <Alert kind="success">Log retention saved.</Alert> : null}
        <div className="form-grid">
          <TextField
            label="Keep log files for (days)"
            value={retentionDays}
            onChange={(value) => {
              setRetentionDays(value);
              setSaved(false);
            }}
            error={errors.retentionDays}
            inputMode="numeric"
            maxLength={3}
            hint="Between 1 and 365 days."
          />
        </div>
        <div className="button-row">
          <button type="submit" className="button button-primary" disabled={busy}>
            {busy ? 'Saving…' : 'Save log retention'}
          </button>
        </div>
      </form>
    </section>
  );
}
