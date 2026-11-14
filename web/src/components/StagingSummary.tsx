import type { StagingStatus } from '../api/types';
import { formatBytes, formatGB } from '../lib/format';
import { Badge } from './Badge';

/** Staging free space against the floor below which emergency cleanup starts. */
export function StagingSummary({ staging }: { staging: StagingStatus }) {
  const measured = staging.freeBytes !== null;
  const aboveFloor = staging.freeBytes !== null && staging.freeBytes >= staging.floorBytes;

  return (
    <dl className="facts">
      <div>
        <dt>Folder</dt>
        <dd className="mono">{staging.path}</dd>
      </div>
      <div>
        <dt>Free space</dt>
        <dd>
          {measured ? (
            <>
              {formatBytes(staging.freeBytes)} free of {formatBytes(staging.totalBytes)}{' '}
              <Badge tone={aboveFloor ? 'green' : 'red'}>{aboveFloor ? 'Above floor' : 'Below floor'}</Badge>
            </>
          ) : (
            <Badge tone="grey">Not measured yet</Badge>
          )}
        </dd>
      </div>
      <div>
        <dt>Floor</dt>
        <dd>
          {formatBytes(staging.floorBytes)} must stay free (the larger of {staging.minFreePercent}% and{' '}
          {formatGB(staging.minFreeGB)})
        </dd>
      </div>
      <div>
        <dt>Recordings in staging</dt>
        <dd>{formatBytes(staging.usedBytes)}</dd>
      </div>
    </dl>
  );
}
