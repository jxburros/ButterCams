import type {
  CameraState,
  CopyState,
  DestinationState,
  HealthStatus,
  Severity,
} from '../api/types';

export type Tone = 'green' | 'blue' | 'amber' | 'red' | 'grey';

export function Badge({ tone, children }: { tone: Tone; children: string }) {
  return <span className={`badge badge-${tone}`}>{children}</span>;
}

const CAMERA_TONES: Record<CameraState, Tone> = {
  recording: 'green',
  connecting: 'blue',
  backoff: 'amber',
  error: 'red',
  disabled: 'grey',
};

const CAMERA_LABELS: Record<CameraState, string> = {
  recording: 'Recording',
  connecting: 'Connecting',
  backoff: 'Backoff',
  error: 'Error',
  disabled: 'Disabled',
};

/** A camera's state; `null` means its worker has not reported yet. */
export function CameraStateBadge({ state }: { state: CameraState | null }) {
  if (state === null) return <Badge tone="grey">No status yet</Badge>;
  return <Badge tone={CAMERA_TONES[state] ?? 'grey'}>{CAMERA_LABELS[state] ?? state}</Badge>;
}

const DESTINATION_TONES: Record<DestinationState, Tone> = {
  ok: 'green',
  retrying: 'amber',
  disabled: 'grey',
};

const DESTINATION_LABELS: Record<DestinationState, string> = {
  ok: 'OK',
  retrying: 'Retrying',
  disabled: 'Disabled',
};

export function DestinationStateBadge({ state }: { state: DestinationState }) {
  return <Badge tone={DESTINATION_TONES[state] ?? 'grey'}>{DESTINATION_LABELS[state] ?? state}</Badge>;
}

const HEALTH_TONES: Record<HealthStatus, Tone> = {
  healthy: 'green',
  warning: 'amber',
  critical: 'red',
};

const HEALTH_LABELS: Record<HealthStatus, string> = {
  healthy: 'Healthy',
  warning: 'Warning',
  critical: 'Critical',
};

export function HealthBadge({ status }: { status: HealthStatus }) {
  return <Badge tone={HEALTH_TONES[status] ?? 'grey'}>{HEALTH_LABELS[status] ?? status}</Badge>;
}

const SEVERITY_TONES: Record<Severity, Tone> = {
  info: 'blue',
  warning: 'amber',
  critical: 'red',
};

const SEVERITY_LABELS: Record<Severity, string> = {
  info: 'Info',
  warning: 'Warning',
  critical: 'Critical',
};

export function SeverityBadge({ severity }: { severity: Severity }) {
  return <Badge tone={SEVERITY_TONES[severity] ?? 'grey'}>{SEVERITY_LABELS[severity] ?? severity}</Badge>;
}

const COPY_TONES: Record<CopyState, Tone> = {
  queued: 'amber',
  copying: 'blue',
  verified: 'green',
  failed: 'red',
  deleted: 'grey',
};

const COPY_LABELS: Record<CopyState, string> = {
  queued: 'Queued',
  copying: 'Copying',
  verified: 'Verified',
  failed: 'Failed',
  deleted: 'Deleted',
};

export function CopyStateBadge({ state }: { state: CopyState }) {
  return <Badge tone={COPY_TONES[state] ?? 'grey'}>{COPY_LABELS[state] ?? state}</Badge>;
}
