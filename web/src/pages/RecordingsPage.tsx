import { useState } from 'react';
import { api } from '../api/client';
import { Alert } from '../components/Alert';
import { useLoad, usePageTitle } from '../lib/hooks';
import { FilesTab } from './recordings/FilesTab';
import { TimelineTab } from './recordings/TimelineTab';

type Tab = 'timeline' | 'files';

/**
 * Recordings: the timeline that plays (spec 16.5) and, as a second tab, the
 * list of hour files with their copies and downloads.
 */
export function RecordingsPage() {
  usePageTitle('Recordings');
  const system = useLoad(api.system);
  const [tab, setTab] = useState<Tab>('timeline');

  // Until the answer is in, assume playback is on: that is the default.
  const playbackOff = system.data !== null && system.data.live?.enabled === false;
  const shown: Tab = playbackOff ? 'files' : tab;

  return (
    <>
      <div className="page-header">
        <div className="page-title">
          <p className="eyebrow">One MP4 file per hour · plays here and in VLC</p>
          <h1>Recordings</h1>
        </div>
      </div>

      {playbackOff ? (
        <Alert kind="info">
          Playback in the browser is switched off on the System page. The hour files can still be downloaded below.
        </Alert>
      ) : (
        <div className="view-tabs" role="tablist" aria-label="Recordings">
          <button
            type="button"
            role="tab"
            id="tab-timeline"
            aria-selected={shown === 'timeline'}
            aria-controls="panel-recordings"
            className={shown === 'timeline' ? 'view-tab view-tab-on' : 'view-tab'}
            onClick={() => setTab('timeline')}
          >
            Timeline
          </button>
          <button
            type="button"
            role="tab"
            id="tab-files"
            aria-selected={shown === 'files'}
            aria-controls="panel-recordings"
            className={shown === 'files' ? 'view-tab view-tab-on' : 'view-tab'}
            onClick={() => setTab('files')}
          >
            Files
          </button>
        </div>
      )}

      <div id="panel-recordings" role="tabpanel" aria-labelledby={shown === 'timeline' ? 'tab-timeline' : 'tab-files'}>
        {shown === 'timeline' ? <TimelineTab /> : <FilesTab />}
      </div>
    </>
  );
}
