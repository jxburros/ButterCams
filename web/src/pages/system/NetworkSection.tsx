import { useState } from 'react';
import type { FormEvent } from 'react';
import { api } from '../../api/client';
import type { ServerSettings, SystemInfo } from '../../api/types';
import { Alert } from '../../components/Alert';
import { CheckboxField, TextAreaField, TextField } from '../../components/Fields';
import type { FieldErrors } from '../../lib/forms';
import { hasErrors, serverErrors } from '../../lib/forms';
import { checkPort, isCidr, isIPAddress, isIPv6 } from '../../lib/validate';

const FIELDS = ['listenAddress', 'port', 'https', 'allowedSubnets'] as const;

interface Values {
  listenAddress: string;
  port: string;
  https: boolean;
  /** One CIDR per line. */
  allowedSubnets: string;
}

function fromServer(server: ServerSettings): Values {
  return {
    listenAddress: server.listenAddress,
    port: String(server.port),
    https: server.https,
    allowedSubnets: (server.allowedSubnets ?? []).join('\n'),
  };
}

function subnetLines(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

function check(values: Values): FieldErrors {
  const errors: FieldErrors = {};
  const address = values.listenAddress.trim();
  if (address === '') {
    errors.listenAddress = 'Enter the address to listen on. Use 0.0.0.0 for every network adapter.';
  } else if (!isIPAddress(address)) {
    errors.listenAddress =
      'Listen address must be an IP address of this server, such as 192.168.1.10, or 0.0.0.0 for every network adapter.';
  }
  const portError = checkPort(values.port);
  if (portError) errors.port = portError;

  const lines = values.allowedSubnets.split(/\r?\n/);
  for (let index = 0; index < lines.length; index += 1) {
    const line = (lines[index] ?? '').trim();
    if (line !== '' && !isCidr(line)) {
      errors.allowedSubnets = `Line ${index + 1}: "${line}" is not a subnet. Write one per line in CIDR form, such as 192.168.1.0/24.`;
      break;
    }
  }
  return errors;
}

/** Where this page will be once the listener has restarted with new settings. */
function pageUrl(server: ServerSettings, addressChanged: boolean): string {
  const anyAddress = server.listenAddress === '0.0.0.0' || server.listenAddress === '::';
  let host = window.location.hostname;
  if (addressChanged && !anyAddress) {
    host = isIPv6(server.listenAddress) ? `[${server.listenAddress}]` : server.listenAddress;
  }
  const scheme = server.https ? 'https' : 'http';
  const defaultPort = server.https ? 443 : 80;
  const port = server.port === defaultPort ? '' : `:${server.port}`;
  return `${scheme}://${host}${port}/system`;
}

interface SectionProps {
  system: SystemInfo;
  onUpdated: (system: SystemInfo) => void;
}

export function NetworkSection({ system, onUpdated }: SectionProps) {
  const [values, setValues] = useState<Values>(() => fromServer(system.server));
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [movedTo, setMovedTo] = useState<string | null>(null);
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

    const before = system.server;
    setBusy(true);
    try {
      const updated = await api.updateSystem({
        server: {
          listenAddress: values.listenAddress.trim(),
          port: Number(values.port.trim()),
          https: values.https,
          allowedSubnets: subnetLines(values.allowedSubnets),
        },
      });
      const after = updated.server;
      const addressChanged = after.listenAddress !== before.listenAddress;
      const listenerChanged = addressChanged || after.port !== before.port || after.https !== before.https;
      onUpdated(updated);
      setValues(fromServer(after));
      setSaved(true);
      setMovedTo(listenerChanged ? pageUrl(after, addressChanged) : null);
    } catch (err) {
      const result = serverErrors(err, FIELDS);
      setErrors(result.fields);
      setFailure(result.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="panel" aria-labelledby="network-heading">
      <h2 id="network-heading">Network</h2>
      <form className="form" onSubmit={(event) => void save(event)} noValidate>
        {failure ? <Alert kind="error">{failure}</Alert> : null}
        {saved && movedTo === null ? <Alert kind="success">Network settings saved.</Alert> : null}
        {movedTo !== null ? (
          <Alert kind="warning">
            Network settings saved. The server restarts its listener in about a second, and this page will then be
            reachable at <a href={movedTo}>{movedTo}</a>. Open that address to continue; you may be asked to sign in
            again.
          </Alert>
        ) : null}
        <div className="form-grid">
          <TextField
            label="Listen address"
            value={values.listenAddress}
            onChange={(listenAddress) => set('listenAddress', listenAddress)}
            error={errors.listenAddress}
            spellCheck={false}
            autoCapitalize="none"
            hint="0.0.0.0 lets other PCs on the network reach this page."
          />
          <TextField
            label="Port"
            value={values.port}
            onChange={(port) => set('port', port)}
            error={errors.port}
            inputMode="numeric"
            maxLength={5}
          />
        </div>
        <CheckboxField
          label="Use HTTPS"
          checked={values.https}
          onChange={(https) => set('https', https)}
          error={errors.https}
          hint="Without HTTPS, passwords and recordings cross the network unencrypted."
        />
        <TextAreaField
          label="Allowed subnets"
          value={values.allowedSubnets}
          onChange={(allowedSubnets) => set('allowedSubnets', allowedSubnets)}
          error={errors.allowedSubnets}
          rows={4}
          spellCheck={false}
          className="mono"
          placeholder="192.168.1.0/24"
          hint="One subnet per line in CIDR form. Leave empty to let any address reach this page (a login is still required). If this PC is outside every subnet listed, it loses access as soon as you save."
        />
        <div className="button-row">
          <button type="submit" className="button button-primary" disabled={busy}>
            {busy ? 'Saving…' : 'Save network settings'}
          </button>
        </div>
      </form>
    </section>
  );
}
