import { useState } from 'react';
import { api, errorMessage } from '../../api/client';
import type { RebuildStatus } from '../../api/types';
import { Alert } from '../../components/Alert';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { formatDateTime } from '../../lib/format';

function segmentCount(count: number): string {
  return count === 1 ? '1 recording' : `${count} recordings`;
}

interface SectionProps {
  /** Progress, refreshed by the page's polling of GET /system. */
  rebuild: RebuildStatus;
  /** Called once a rebuild has been started, so the page refreshes straight away. */
  onStarted: () => void;
}

export function RebuildSection({ rebuild, onStarted }: SectionProps) {
  const [confirming, setConfirming] = useState(false);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);

  async function start() {
    setStarting(true);
    setStartError(null);
    try {
      await api.rebuildIndex();
      setConfirming(false);
      onStarted();
    } catch (err) {
      setStartError(errorMessage(err));
    } finally {
      setStarting(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="rebuild-heading">
      <h2 id="rebuild-heading">Recording index</h2>
      <p className="muted">
        Rebuilding scans staging and every destination for ButterCams recordings and rebuilds the list shown on the
        Recordings page. Recording continues while it runs.
      </p>

      {rebuild.running ? (
        <Alert kind="info">
          Rebuild running since {formatDateTime(rebuild.startedAt)}: {segmentCount(rebuild.segments)} indexed so far.
        </Alert>
      ) : rebuild.error !== '' ? (
        <Alert kind="error">
          The last rebuild{rebuild.finishedAt ? ` (${formatDateTime(rebuild.finishedAt)})` : ''} failed: {rebuild.error}
        </Alert>
      ) : rebuild.finishedAt ? (
        <Alert kind="success">
          The last rebuild finished {formatDateTime(rebuild.finishedAt)} and indexed {segmentCount(rebuild.segments)}.
        </Alert>
      ) : null}

      <div className="button-row">
        <button
          type="button"
          className="button"
          disabled={rebuild.running}
          onClick={() => {
            setStartError(null);
            setConfirming(true);
          }}
        >
          {rebuild.running ? 'Rebuild running…' : 'Rebuild index'}
        </button>
      </div>

      {confirming ? (
        <ConfirmDialog
          title="Rebuild the recording index?"
          confirmLabel="Rebuild index"
          busy={starting}
          error={startError}
          onConfirm={() => void start()}
          onCancel={() => setConfirming(false)}
        >
          <p>
            The index is rebuilt from the files found on disk. With many recordings this can take a while; progress is
            shown here.
          </p>
        </ConfirmDialog>
      ) : null}
    </section>
  );
}
