import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errorMessage } from '../../api/client';
import type { CertificateInfo, CertificateUpload } from '../../api/types';
import { Alert } from '../../components/Alert';
import { FilePicker, TextAreaField, TextField } from '../../components/Fields';
import { formatDateTime } from '../../lib/format';
import type { FieldErrors } from '../../lib/forms';
import { hasErrors, readFileBase64, readTextFile, serverErrors } from '../../lib/forms';

const FIELDS = ['certPem', 'keyPem', 'pfxBase64', 'password'] as const;
const MAX_CERT_BYTES = 1024 * 1024;

type Format = 'pem' | 'pfx';

interface SectionProps {
  certificate: CertificateInfo | null;
  onUploaded: (certificate: CertificateInfo) => void;
}

export function CertificateSection({ certificate, onUploaded }: SectionProps) {
  const [format, setFormat] = useState<Format>('pem');
  const [certPem, setCertPem] = useState('');
  const [keyPem, setKeyPem] = useState('');
  const [pfxFile, setPfxFile] = useState<File | null>(null);
  const [password, setPassword] = useState('');
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  // Remounts the file inputs after an upload so they show no stale file name.
  const [pickerKey, setPickerKey] = useState(0);

  async function loadPem(file: File | null, field: 'certPem' | 'keyPem') {
    if (file === null) return;
    try {
      const text = await readTextFile(file, MAX_CERT_BYTES);
      if (field === 'certPem') setCertPem(text);
      else setKeyPem(text);
      setErrors((current) => {
        const { [field]: _removed, ...rest } = current;
        return rest;
      });
    } catch (err) {
      setErrors((current) => ({ ...current, [field]: errorMessage(err) }));
    }
  }

  async function buildUpload(): Promise<CertificateUpload | null> {
    const found: FieldErrors = {};
    if (format === 'pem') {
      if (!certPem.includes('-----BEGIN CERTIFICATE-----')) {
        found.certPem = 'Paste the certificate in PEM form. It starts with -----BEGIN CERTIFICATE-----.';
      }
      if (!/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(keyPem)) {
        found.keyPem = 'Paste the private key in PEM form. It starts with a line such as -----BEGIN PRIVATE KEY-----.';
      }
      setErrors(found);
      return hasErrors(found) ? null : { certPem: certPem.trim(), keyPem: keyPem.trim() };
    }

    if (pfxFile === null) {
      found.pfxBase64 = 'Choose the .pfx or .p12 file that holds the certificate and its private key.';
      setErrors(found);
      return null;
    }
    try {
      const pfxBase64 = await readFileBase64(pfxFile, MAX_CERT_BYTES);
      setErrors(found);
      return { pfxBase64, password };
    } catch (err) {
      found.pfxBase64 = errorMessage(err);
      setErrors(found);
      return null;
    }
  }

  async function upload(event: FormEvent) {
    event.preventDefault();
    setFailure(null);
    setSaved(false);
    const body = await buildUpload();
    if (body === null) return;

    setBusy(true);
    try {
      const response = await api.uploadCertificate(body);
      onUploaded(response.certificate);
      setCertPem('');
      setKeyPem('');
      setPfxFile(null);
      setPassword('');
      setPickerKey((key) => key + 1);
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
    <section className="panel" aria-labelledby="certificate-heading">
      <h2 id="certificate-heading">HTTPS certificate</h2>
      {certificate ? (
        <dl className="facts">
          <div>
            <dt>Subject</dt>
            <dd className="mono">{certificate.subject}</dd>
          </div>
          <div>
            <dt>Expires</dt>
            <dd>{formatDateTime(certificate.notAfter)}</dd>
          </div>
          <div>
            <dt>Self-signed</dt>
            <dd>{certificate.selfSigned ? 'Yes (browsers show a warning until it is trusted)' : 'No'}</dd>
          </div>
          <div>
            <dt>Names</dt>
            <dd>{(certificate.dnsNames ?? []).length > 0 ? certificate.dnsNames.join(', ') : '–'}</dd>
          </div>
          <div>
            <dt>SHA-256 fingerprint</dt>
            <dd className="mono wrap-anywhere">{certificate.fingerprintSha256}</dd>
          </div>
        </dl>
      ) : (
        <p className="muted">No certificate is installed.</p>
      )}

      <form className="form" onSubmit={(event) => void upload(event)} noValidate>
        <h3>Replace the certificate</h3>
        {failure ? <Alert kind="error">{failure}</Alert> : null}
        {saved ? (
          <Alert kind="success">
            The certificate was installed. Reload this page if the browser still shows the old one.
          </Alert>
        ) : null}

        <fieldset>
          <legend>Format</legend>
          <div className="check-row">
            <input
              id="certificate-format-pem"
              type="radio"
              name="certificate-format"
              checked={format === 'pem'}
              onChange={() => {
                setFormat('pem');
                setPfxFile(null);
                setErrors({});
              }}
            />
            <label htmlFor="certificate-format-pem">PEM certificate and private key</label>
          </div>
          <div className="check-row">
            <input
              id="certificate-format-pfx"
              type="radio"
              name="certificate-format"
              checked={format === 'pfx'}
              onChange={() => {
                setFormat('pfx');
                setErrors({});
              }}
            />
            <label htmlFor="certificate-format-pfx">PFX file (.pfx or .p12)</label>
          </div>
        </fieldset>

        {format === 'pem' ? (
          <div className="form-grid">
            <div>
              <TextAreaField
                label="Certificate (PEM)"
                value={certPem}
                onChange={setCertPem}
                error={errors.certPem}
                rows={6}
                spellCheck={false}
                className="mono"
                placeholder="-----BEGIN CERTIFICATE-----"
              />
              <FilePicker
                key={`cert-${pickerKey}`}
                label="Or load the certificate from a file"
                accept=".pem,.crt,.cer,.txt"
                disabled={busy}
                onPick={(file) => void loadPem(file, 'certPem')}
              />
            </div>
            <div>
              <TextAreaField
                label="Private key (PEM)"
                value={keyPem}
                onChange={setKeyPem}
                error={errors.keyPem}
                rows={6}
                spellCheck={false}
                className="mono"
                placeholder="-----BEGIN PRIVATE KEY-----"
              />
              <FilePicker
                key={`key-${pickerKey}`}
                label="Or load the private key from a file"
                accept=".pem,.key,.txt"
                disabled={busy}
                onPick={(file) => void loadPem(file, 'keyPem')}
              />
            </div>
          </div>
        ) : (
          <div className="form-grid">
            <FilePicker
              key={`pfx-${pickerKey}`}
              label="PFX file"
              accept=".pfx,.p12,application/x-pkcs12"
              disabled={busy}
              onPick={setPfxFile}
              error={errors.pfxBase64}
            />
            <TextField
              label="PFX password"
              type="password"
              value={password}
              onChange={setPassword}
              error={errors.password}
              autoComplete="new-password"
              hint="Leave blank if the file has no password."
            />
          </div>
        )}

        <div className="button-row">
          <button type="submit" className="button button-primary" disabled={busy}>
            {busy ? 'Installing…' : 'Install certificate'}
          </button>
        </div>
      </form>
    </section>
  );
}
