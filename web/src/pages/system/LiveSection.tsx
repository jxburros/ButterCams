import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errorMessage } from '../../api/client';
import type { SystemInfo } from '../../api/types';
import { Alert } from '../../components/Alert';
import { CheckboxField } from '../../components/Fields';

interface SectionProps {
  system: SystemInfo;
  onUpdated: (system: SystemInfo) => void;
}

/** The two switches of live view and playback (spec 16.8). Neither touches a recording. */
export function LiveSection({ system, onUpdated }: SectionProps) {
  const [enabled, setEnabled] = useState(system.live.enabled);
  const [mainStream, setMainStream] = useState(system.live.mainStream);
  const [failure, setFailure] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  async function save(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    setSaved(false);
    setBusy(true);
    try {
      const updated = await api.updateSystem({ live: { enabled, mainStream } });
      onUpdated(updated);
      setEnabled(updated.live.enabled);
      setMainStream(updated.live.mainStream);
      setSaved(true);
    } catch (err) {
      setFailure(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="live-settings-heading">
      <h2 id="live-settings-heading">Live view and playback</h2>
      <form className="form" onSubmit={(event) => void save(event)} noValidate>
        {failure ? <Alert kind="error">{failure}</Alert> : null}
        {saved ? <Alert kind="success">Live view settings saved. Recording was not interrupted.</Alert> : null}
        <CheckboxField
          label="Live view and playback in the browser"
          checked={enabled}
          onChange={(value) => {
            setEnabled(value);
            setSaved(false);
          }}
          hint="Off stops the live view process: the Live page shows camera states only and recordings can be downloaded but not played. Recording carries on either way."
        />
        <CheckboxField
          label="Full-quality main stream in live view"
          checked={mainStream}
          disabled={!enabled}
          onChange={(value) => {
            setMainStream(value);
            setSaved(false);
          }}
          hint="On, a camera opened in single view (and any camera without a sub stream) shows the stream that is being recorded, passed on by the recorder. Off, live view uses sub streams only and the recorders offer nothing to viewers."
        />
        <div className="button-row">
          <button type="submit" className="button button-primary" disabled={busy}>
            {busy ? 'Saving…' : 'Save live view settings'}
          </button>
        </div>
      </form>
    </section>
  );
}
