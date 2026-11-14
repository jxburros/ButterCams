import { useState } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { REFRESH_MS, useLoad, usePageTitle } from '../lib/hooks';
import { AboutSection } from './system/AboutSection';
import { CertificateSection } from './system/CertificateSection';
import { ConfigSection } from './system/ConfigSection';
import { LoggingSection } from './system/LoggingSection';
import { NetworkSection } from './system/NetworkSection';
import { RebuildSection } from './system/RebuildSection';
import { StagingSection } from './system/StagingSection';
import { UsersSection } from './system/UsersSection';

/** Headings of the sections below, for the jump links at the top. */
const SECTIONS: [string, string][] = [
  ['staging-settings-heading', 'Staging'],
  ['network-heading', 'Network'],
  ['certificate-heading', 'Certificate'],
  ['users-heading', 'Users'],
  ['logging-heading', 'Logging'],
  ['config-heading', 'Configuration'],
  ['rebuild-heading', 'Rebuild index'],
  ['about-heading', 'Version and uptime'],
];

/** System settings. Admins only (the route guard keeps viewers out). */
export function SystemPage() {
  usePageTitle('System');
  // Polled so that uptime and rebuild progress stay current. The settings
  // forms keep their own values and are not overwritten by a refresh.
  const system = useLoad(api.system, REFRESH_MS);
  // Bumped after a configuration import so the forms start again from the imported values.
  const [revision, setRevision] = useState(0);

  const info = system.data;

  async function afterImport() {
    await system.reload();
    setRevision((current) => current + 1);
  }

  return (
    <>
      <div className="page-header">
        <div className="page-title">
          <p className="eyebrow">Settings for this server</p>
          <h1>System</h1>
        </div>
      </div>

      {system.error ? <Alert kind="error">System information could not be refreshed: {system.error}</Alert> : null}

      {info === null ? (
        system.loading ? (
          <p className="muted">Loading…</p>
        ) : null
      ) : (
        <div className="system-sections">
          <nav aria-label="System sections">
            <ul className="jump-nav">
              {SECTIONS.map(([id, label]) => (
                <li key={id}>
                  <a href={`#${id}`}>{label}</a>
                </li>
              ))}
            </ul>
          </nav>
          <StagingSection key={`staging-${revision}`} system={info} onUpdated={system.replace} />
          <NetworkSection key={`network-${revision}`} system={info} onUpdated={system.replace} />
          <CertificateSection
            certificate={info.certificate}
            onUploaded={(certificate) => system.replace({ ...info, certificate })}
          />
          <UsersSection />
          <LoggingSection key={`logging-${revision}`} system={info} onUpdated={system.replace} />
          <ConfigSection onImported={() => void afterImport()} />
          <RebuildSection rebuild={info.rebuild} onStarted={() => void system.reload()} />
          <AboutSection system={info} />
        </div>
      )}
    </>
  );
}
