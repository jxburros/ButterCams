import { useState } from 'react';
import { api, errorMessage } from '../../api/client';
import { Alert } from '../../components/Alert';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { FilePicker } from '../../components/Fields';
import { readTextFile } from '../../lib/forms';

const MAX_CONFIG_BYTES = 10 * 1024 * 1024;

/** Hands a file to the browser's download handling. */
function saveFile(blob: Blob, fileName: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = fileName;
  document.body.appendChild(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

interface PendingImport {
  fileName: string;
  /** The parsed config document, sent as the request body. */
  config: unknown;
}

interface SectionProps {
  /** Called after a configuration was imported, so the page can show the new settings. */
  onImported: () => void;
}

export function ConfigSection({ onImported }: SectionProps) {
  const [exportError, setExportError] = useState<string | null>(null);
  const [exporting, setExporting] = useState(false);

  const [fileError, setFileError] = useState<string | undefined>(undefined);
  const [pending, setPending] = useState<PendingImport | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const [imported, setImported] = useState<string | null>(null);
  // Remounts the file input after an import so it shows no stale file name.
  const [pickerKey, setPickerKey] = useState(0);

  async function exportConfig() {
    setExportError(null);
    setExporting(true);
    try {
      saveFile(await api.exportConfig(), 'config.json');
    } catch (err) {
      setExportError(errorMessage(err));
    } finally {
      setExporting(false);
    }
  }

  async function pickFile(file: File | null) {
    setFileError(undefined);
    setImported(null);
    setPending(null);
    if (file === null) return;
    try {
      const text = await readTextFile(file, MAX_CONFIG_BYTES);
      let config: unknown;
      try {
        config = JSON.parse(text);
      } catch {
        setFileError(`"${file.name}" is not valid JSON. Choose a config.json exported from ButterCams.`);
        return;
      }
      if (typeof config !== 'object' || config === null || Array.isArray(config)) {
        setFileError(`"${file.name}" is not a ButterCams configuration. Choose a config.json exported from ButterCams.`);
        return;
      }
      setPending({ fileName: file.name, config });
    } catch (err) {
      setFileError(errorMessage(err));
    }
  }

  async function importConfig() {
    if (pending === null) return;
    setImporting(true);
    setImportError(null);
    try {
      await api.importConfig(pending.config);
      setConfirming(false);
      setImported(`The configuration in ${pending.fileName} was imported.`);
      setPending(null);
      setPickerKey((key) => key + 1);
      onImported();
    } catch (err) {
      setImportError(errorMessage(err));
    } finally {
      setImporting(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="config-heading">
      <h2 id="config-heading">Configuration file</h2>

      <h3>Export</h3>
      <p className="muted">Downloads config.json with every password removed.</p>
      {exportError ? <Alert kind="error">The configuration could not be exported: {exportError}</Alert> : null}
      <div className="button-row">
        <button type="button" className="button" disabled={exporting} onClick={() => void exportConfig()}>
          {exporting ? 'Exporting…' : 'Export configuration'}
        </button>
      </div>

      <h3>Import</h3>
      <p className="muted">
        Replaces the current configuration with the one in the file. Saved passwords are kept for cameras and
        destinations whose ID already exists; new ones need their password entered afterwards.
      </p>
      {imported ? <Alert kind="success">{imported}</Alert> : null}
      <div className="form">
        <FilePicker
          key={pickerKey}
          label="Configuration file (config.json)"
          accept=".json,application/json"
          disabled={importing}
          onPick={(file) => void pickFile(file)}
          error={fileError}
        />
        <div className="button-row">
          <button
            type="button"
            className="button button-primary"
            disabled={pending === null || importing}
            onClick={() => {
              setImportError(null);
              setConfirming(true);
            }}
          >
            Import configuration
          </button>
        </div>
      </div>

      {confirming && pending !== null ? (
        <ConfirmDialog
          title="Import this configuration?"
          confirmLabel="Import configuration"
          danger
          busy={importing}
          error={importError}
          onConfirm={() => void importConfig()}
          onCancel={() => setConfirming(false)}
        >
          <p>
            The settings in <span className="mono">{pending.fileName}</span> replace the current cameras, destinations
            and system settings. Cameras that changed are restarted.
          </p>
        </ConfirmDialog>
      ) : null}
    </section>
  );
}
