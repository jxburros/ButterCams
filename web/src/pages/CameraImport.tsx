import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errorMessage } from '../api/client';
import type { CameraImportResponse } from '../api/types';
import { Alert } from '../components/Alert';
import { Badge } from '../components/Badge';
import { FilePicker, TextAreaField } from '../components/Fields';
import { readTextFile } from '../lib/forms';

const HEADER = 'name,host,port,path,username,password,destinations';
const MAX_CSV_BYTES = 2 * 1024 * 1024;

/** Returns a message when the text cannot be a camera CSV. */
function checkCsv(csv: string): string | null {
  const lines = csv.split(/\r?\n/).filter((line) => line.trim() !== '');
  const first = lines[0];
  if (first === undefined) return 'Paste the CSV text or choose a CSV file.';
  if (first.replace(/\s+/g, '').toLowerCase() !== HEADER) {
    return `The first line must be the header: ${HEADER}`;
  }
  if (lines.length < 2) return 'Add at least one camera line below the header.';
  return null;
}

/** One entry of a result's `errors`, as text. */
function problemText(problem: unknown): string {
  if (typeof problem === 'string') return problem;
  if (typeof problem === 'object' && problem !== null && 'message' in problem) {
    return String(problem.message);
  }
  return JSON.stringify(problem);
}

interface CameraImportProps {
  /** Called after an import that created at least one camera. */
  onImported: () => void;
  onClose: () => void;
}

export function CameraImport({ onImported, onClose }: CameraImportProps) {
  const [csv, setCsv] = useState(`${HEADER}\n`);
  const [csvError, setCsvError] = useState<string | undefined>(undefined);
  const [fileError, setFileError] = useState<string | undefined>(undefined);
  const [failure, setFailure] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<CameraImportResponse | null>(null);

  async function pickFile(file: File | null) {
    setFileError(undefined);
    if (file === null) return;
    try {
      setCsv(await readTextFile(file, MAX_CSV_BYTES));
      setCsvError(undefined);
    } catch (err) {
      setFileError(errorMessage(err));
    }
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    setResult(null);
    const text = csv.replace(/^﻿/, '');
    const problem = checkCsv(text);
    setCsvError(problem ?? undefined);
    if (problem) return;

    setBusy(true);
    try {
      const response = await api.importCameras({ csv: text });
      setResult(response);
      if (response.created > 0) onImported();
    } catch (err) {
      setFailure(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  const results = result?.results ?? [];
  const rejected = results.filter((line) => !line.ok).length;

  return (
    <form className="panel form" onSubmit={(event) => void submit(event)} noValidate>
      <h2>Bulk import cameras</h2>
      <p>
        One camera per line, in this column order: <code>{HEADER}</code>. Separate several destination IDs with a
        semicolon (<code>;</code>).
      </p>
      {failure ? <Alert kind="error">{failure}</Alert> : null}

      <FilePicker
        label="CSV file"
        accept=".csv,text/csv,text/plain"
        disabled={busy}
        onPick={(file) => void pickFile(file)}
        error={fileError}
        hint="Choosing a file replaces the text below."
      />
      <TextAreaField
        label="CSV text"
        value={csv}
        onChange={setCsv}
        error={csvError}
        rows={10}
        spellCheck={false}
        className="mono"
      />

      <div className="button-row">
        <button type="submit" className="button button-primary" disabled={busy}>
          {busy ? 'Importing…' : 'Import cameras'}
        </button>
        <button type="button" className="button" disabled={busy} onClick={onClose}>
          Back to cameras
        </button>
      </div>

      {result ? (
        <div className="import-result">
          <Alert kind={rejected > 0 ? 'warning' : 'success'}>
            {result.created === 1 ? '1 camera was created.' : `${result.created} cameras were created.`}
            {rejected > 0
              ? ` ${rejected === 1 ? '1 line was' : `${rejected} lines were`} not imported; the reasons are listed below.`
              : ''}
          </Alert>
          {results.length > 0 ? (
            <div className="table-wrap">
              <table>
                <caption className="visually-hidden">Result for each CSV line</caption>
                <thead>
                  <tr>
                    <th scope="col" className="num">
                      Line
                    </th>
                    <th scope="col">Name</th>
                    <th scope="col">ID</th>
                    <th scope="col">Result</th>
                    <th scope="col">Problems</th>
                  </tr>
                </thead>
                <tbody>
                  {results.map((line) => (
                    <tr key={line.line}>
                      <td className="num">{line.line}</td>
                      <td>{line.name}</td>
                      <td className="mono">{line.id}</td>
                      <td>
                        <Badge tone={line.ok ? 'green' : 'red'}>{line.ok ? 'Created' : 'Not imported'}</Badge>
                      </td>
                      <td>
                        {(line.errors ?? []).length > 0 ? (
                          <ul className="plain-list">
                            {(line.errors ?? []).map((problem, index) => (
                              <li key={index}>{problemText(problem)}</li>
                            ))}
                          </ul>
                        ) : (
                          '–'
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </div>
      ) : null}
    </form>
  );
}
