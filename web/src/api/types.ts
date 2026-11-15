// Every JSON shape of the ButterCams REST API (v1), as written in docs/API.md.
// Keep this file in step with that document; nothing else in the UI declares
// API shapes.

// ---------------------------------------------------------------- Conventions

/** RFC 3339 timestamp string. */
export type Timestamp = string;

export interface FieldError {
  field: string;
  message: string;
}

export type ApiErrorCode =
  | 'validation_failed'
  | 'unauthorized'
  | 'invalid_credentials'
  | 'account_locked'
  | 'forbidden'
  | 'csrf_failed'
  | 'not_found'
  | 'conflict'
  | 'timeout'
  | 'internal';

/** Body of every non-2xx response. `fields` is present only for `validation_failed`. */
export interface ErrorEnvelope {
  error: {
    code: ApiErrorCode;
    message: string;
    fields?: FieldError[];
  };
}

export type Severity = 'info' | 'warning' | 'critical';

// ----------------------------------------------------------------------- Auth

export type Role = 'admin' | 'viewer';

export interface User {
  username: string;
  role: Role;
}

/** POST /auth/login body. */
export interface LoginRequest {
  username: string;
  password: string;
}

/** Response of POST /auth/login and GET /auth/session. */
export interface Session {
  user: User;
  csrfToken: string;
}

// --------------------------------------------------------------------- Health

export type HealthStatus = 'healthy' | 'warning' | 'critical';

export interface HealthReason {
  severity: Severity;
  code: string;
  message: string;
  /** Set when the reason is about one camera. */
  cameraId?: string | null;
}

/** GET /health. */
export interface Health {
  status: HealthStatus;
  reasons: HealthReason[];
  checkedAt: Timestamp;
  version: string;
  uptimeSec: number;
}

// -------------------------------------------------------------------- Cameras

export type CameraState = 'disabled' | 'connecting' | 'recording' | 'backoff' | 'error';

export type Transport = 'tcp' | 'udp';

export interface CameraStats {
  state: CameraState;
  stateSince: Timestamp | null;
  /** Reason for backoff / error. */
  message: string;
  lastPacketTs: Timestamp | null;
  /** Age of the last video packet when the recorder last reported, in seconds (server clock). */
  lastPacketAgeSec: number | null;
  /** Seconds since the recorder last reported on this camera (server clock). */
  reportAgeSec: number;
  videoCodec: string;
  audioCodec: string;
  /** For example "audio not recorded: speex". */
  audioNote: string;
  width: number;
  height: number;
  fps: number;
  keyframeIntervalMs: number;
  bitrateKbps: number;
  currentFile: string;
  currentFileBytes: number;
  singleSession: boolean;
  workerId: number;
  updatedAt: Timestamp | null;
}

export interface Camera {
  id: string;
  name: string;
  enabled: boolean;
  host: string;
  port: number;
  path: string;
  username: string;
  hasPassword: boolean;
  transport: Transport;
  recordAudio: boolean;
  destinationIds: string[];
  /** Path of the low-resolution sub stream used by the live grid; `null` = none. */
  subStreamPath: string | null;
  /** Reserved for later versions; v1 is always "continuous". */
  recordingMode: string;
  /** Reserved for later versions. */
  extensions: Record<string, unknown>;
  /** `null` until the camera's worker has reported. */
  stats: CameraStats | null;
}

/** GET /cameras. */
export interface CamerasResponse {
  cameras: Camera[];
}

/** Body of POST /cameras, PUT /cameras/{id} and POST /cameras/test. */
export interface CameraInput {
  /**
   * Optional on create (generated from name); ignored on update. On
   * POST /cameras/test it names the saved camera whose password is reused.
   */
  id?: string;
  name: string;
  enabled: boolean;
  /** Optional full rtsp:// URL; when set it overrides host/port/path. */
  url?: string;
  host: string;
  port: number;
  path: string;
  username: string;
  /** Omit or "" to keep the saved password. */
  password?: string;
  /** `true` removes the saved password. */
  clearPassword?: boolean;
  transport: Transport;
  recordAudio: boolean;
  destinationIds: string[];
  /** Omit to keep the saved value; "" removes the sub stream. */
  subStreamPath?: string;
}

/** Response of POST /cameras/test. */
export interface CameraTestResult {
  ok: boolean;
  error: string;
  videoCodec: string;
  width: number;
  height: number;
  fps: number;
  keyframeIntervalMs: number;
  audioCodec: string;
  audioNote: string;
  /** `null` when it could not be determined. */
  secondSessionAllowed: boolean | null;
  warnings: string[];
  /** Result for the sub stream; `null` when the camera has none. */
  sub: SubStreamTestResult | null;
}

export interface SubStreamTestResult {
  ok: boolean;
  error: string;
  videoCodec: string;
  width: number;
  height: number;
  warnings: string[];
}

/** Body of POST /cameras/import. */
export interface CameraImportRequest {
  /** CSV text: `name,host,port,path,username,password,destinations` (destinations separated by `;`). */
  csv: string;
}

export interface CameraImportLineResult {
  line: number;
  name: string;
  id: string;
  ok: boolean;
  errors: string[];
}

/** Response of POST /cameras/import. */
export interface CameraImportResponse {
  created: number;
  results: CameraImportLineResult[];
}

// --------------------------------------------------------------- Destinations

export type DestinationType = 'local' | 'unc';

export type DestinationState = 'ok' | 'retrying' | 'disabled';

export interface DestinationStatus {
  state: DestinationState;
  lastError: string;
  queueLength: number;
  oldestQueuedTs: Timestamp | null;
  usedBytes: number;
  /** `null` until first measured. */
  freeBytes: number | null;
  /** `null` until first measured. */
  totalBytes: number | null;
  lastSuccessTs: Timestamp | null;
}

export interface Destination {
  id: string;
  name: string;
  type: DestinationType;
  path: string;
  username: string;
  hasPassword: boolean;
  capGB: number;
  maxAgeDays: number | null;
  enabled: boolean;
  status: DestinationStatus;
}

export interface StagingStatus {
  path: string;
  minFreePercent: number;
  minFreeGB: number;
  /** `null` until first measured. */
  freeBytes: number | null;
  /** `null` until first measured. */
  totalBytes: number | null;
  floorBytes: number;
  usedBytes: number;
}

/** GET /destinations. */
export interface DestinationsResponse {
  destinations: Destination[];
  staging: StagingStatus;
}

/** Body of POST /destinations, PUT /destinations/{id} and POST /destinations/test. */
export interface DestinationInput {
  /** On POST /destinations/test it names the saved destination whose password is reused. */
  id?: string;
  name: string;
  type: DestinationType;
  path: string;
  username: string;
  password?: string;
  clearPassword?: boolean;
  capGB: number;
  maxAgeDays: number | null;
  enabled: boolean;
}

/** Response of POST /destinations/test. */
export interface DestinationTestResult {
  ok: boolean;
  error: string;
  freeBytes: number | null;
  totalBytes: number | null;
  durationMs: number;
}

// ----------------------------------------------------------------- Recordings

export interface RecordingCamera {
  id: string;
  name: string;
  /** `true` when the camera is no longer configured but still has indexed recordings. */
  deleted: boolean;
}

/** GET /recordings/cameras. */
export interface RecordingCamerasResponse {
  cameras: RecordingCamera[];
}

export type CopyState = 'queued' | 'copying' | 'verified' | 'failed' | 'deleted';

export interface SegmentCopy {
  /** A destination id, or "staging" for the staging copy. */
  destinationId: string;
  state: CopyState;
  lastError: string;
  updatedTs: Timestamp;
}

export interface Segment {
  id: number;
  cameraId: string;
  fileName: string;
  start: Timestamp;
  /** `null` for the file currently being written. */
  end: Timestamp | null;
  /** `null` for the file currently being written. */
  bytes: number | null;
  videoCodec: string;
  audioCodec: string;
  gapBefore: boolean;
  /** hour_rollover | stall | shutdown | crash_recovered | param_change | clock_jump. */
  closeReason: string | null;
  protected: boolean;
  copies: SegmentCopy[];
}

/** GET /segments?camera=<id>&date=YYYY-MM-DD. */
export interface SegmentsResponse {
  segments: Segment[];
}

// --------------------------------------------------------------------- Events

export interface EventRow {
  id: number;
  ts: Timestamp;
  /** guardian | worker:N | mover | camera:<id>. */
  source: string;
  type: string;
  severity: Severity;
  cameraId: string | null;
  data: Record<string, unknown> | null;
}

/** GET /events. */
export interface EventsResponse {
  events: EventRow[];
}

/** Query parameters of GET /events. */
export interface EventsQuery {
  since?: Timestamp;
  severity?: Severity;
  camera?: string;
  /** Defaults to 200, maximum 1000. */
  limit?: number;
  /** Pages backwards by event id. */
  before?: number;
}

// --------------------------------------------------------------------- System

export interface ServerSettings {
  listenAddress: string;
  port: number;
  https: boolean;
  /** CIDR list; empty means any address can reach the UI. */
  allowedSubnets: string[];
}

export interface StagingSettings {
  path: string;
  minFreePercent: number;
  minFreeGB: number;
}

export interface SystemStaging extends StagingSettings {
  freeBytes: number | null;
  totalBytes: number | null;
}

export interface LoggingSettings {
  retentionDays: number;
}

export interface CertificateInfo {
  subject: string;
  notAfter: Timestamp;
  selfSigned: boolean;
  dnsNames: string[];
  fingerprintSha256: string;
}

export interface SystemCounts {
  cameras: number;
  enabledCameras: number;
  destinations: number;
  workers: number;
}

export interface RebuildStatus {
  running: boolean;
  startedAt: Timestamp | null;
  finishedAt: Timestamp | null;
  error: string;
  segments: number;
}

/** GET /system, and the response of PUT /system. */
export interface SystemInfo {
  version: string;
  startedAt: Timestamp;
  uptimeSec: number;
  hostname: string;
  os: string;
  dataRoot: string;
  schemaVersion: number;
  server: ServerSettings;
  staging: SystemStaging;
  logging: LoggingSettings;
  certificate: CertificateInfo | null;
  counts: SystemCounts;
  rebuild: RebuildStatus;
  live: LiveSettings;
  media: MediaStatus;
}

/** Live view and playback switches (spec 16.8). */
export interface LiveSettings {
  /** Off stops the media process and hides live view and playback. */
  enabled: boolean;
  /** Off keeps the recorders' main-stream restream off; live uses sub streams only. */
  mainStream: boolean;
}

export interface MediaSource {
  cameraId: string;
  stream: StreamKind;
  viewers: number;
  state: 'playing' | 'connecting' | 'waiting';
}

/** State of the media process (live view and playback server). */
export interface MediaStatus {
  enabled: boolean;
  state: 'running' | 'starting' | 'off';
  viewers: number;
  /** Empty for Viewers with a camera limit. */
  sources: MediaSource[];
  overflows: number;
  memBytes: number;
}

/** Body of PUT /system: any of the three sections. */
export interface SystemUpdate {
  server?: ServerSettings;
  staging?: StagingSettings;
  logging?: LoggingSettings;
  live?: Partial<LiveSettings>;
}

/** Response of POST /system/rebuild-index (202). */
export interface RebuildStarted {
  started: boolean;
}

/** Body of POST /system/certificate: a PEM pair or a PFX file. */
export type CertificateUpload =
  | { certPem: string; keyPem: string }
  | { pfxBase64: string; password: string };

/** Response of POST /system/certificate. */
export interface CertificateResponse {
  certificate: CertificateInfo;
}

// ---------------------------------------------------------------------- Users

export interface UserAccount {
  username: string;
  role: Role;
  createdAt: Timestamp;
  locked: boolean;
  /** `null` = every camera; otherwise the cameras this Viewer may see. */
  cameras: string[] | null;
}

/** GET /users. */
export interface UsersResponse {
  users: UserAccount[];
}

/** Body of POST /users. */
export interface UserCreate {
  username: string;
  password: string;
  role: Role;
  /** Camera ids a Viewer is limited to; `null` or omitted = every camera. */
  cameras?: string[] | null;
}

/** Body of PUT /users/{username}. */
export interface UserUpdate {
  password?: string;
  role?: Role;
  /** Omit to keep; `null` removes the limit; a list sets it. */
  cameras?: string[] | null;
}

// ---------------------------------------------------------------- Live view

export type StreamKind = 'main' | 'sub';

export type TileLayout = 1 | 4 | 9 | 16;

/** A saved arrangement of cameras on the Live page. */
export interface LiveView {
  id: string;
  name: string;
  layout: TileLayout;
  /** Camera ids in tile order; only cameras the user may see. */
  cameraIds: string[];
}

export interface LiveViewsResponse {
  /** Views every user sees; only Admins edit them. */
  shared: LiveView[];
  /** The signed-in user's own views. */
  mine: LiveView[];
}

/** First (text) message of the live WebSocket, and again when the stream's parameters change. */
export interface LiveInitMessage {
  type: 'init';
  /** Full MIME type for MediaSource.addSourceBuffer, e.g. `video/mp4; codecs="avc1.64001f"`. */
  mime: string;
  width: number;
  height: number;
}

/** Sent while there is no video (camera offline, connecting …). */
export interface LiveStateMessage {
  type: 'state';
  state: 'waiting';
  message: string;
}

export type LiveMessage = LiveInitMessage | LiveStateMessage;

// ---------------------------------------------------------------- Timeline

export interface TimelineSegment {
  id: number;
  start: Timestamp;
  /** `null` while the file is being recorded. */
  end: Timestamp | null;
  /** `true` when recording was interrupted before this file. */
  gapBefore: boolean;
  videoCodec: string;
  audioCodec: string;
  /** `true` for the file being recorded right now. */
  open: boolean;
  fileName: string;
  bytes: number | null;
}

export interface TimelineResponse {
  camera: string;
  from: Timestamp;
  to: Timestamp;
  /** The server's clock when it answered. */
  now: Timestamp;
  segments: TimelineSegment[];
  /** Recorder events of this camera in the range (stalls, reconnects, clock changes), oldest first. */
  events: EventRow[];
}

/** One keyframe-aligned chunk of a recording: [startMs, durationMs, byteOffset, byteSize]. */
export type PlayChunk = [number, number, number, number];

/** GET /segments/{id}/play/index. Times are milliseconds from the start of the file. */
export interface PlayIndex {
  /** Full MIME type for MediaSource.addSourceBuffer. */
  mime: string;
  /** Byte range [offset, length] of the initialization segment. */
  init: [number, number];
  /** Wall-clock time of the first frame. */
  start: Timestamp;
  /** Length of the video in the file so far. */
  durationMs: number;
  chunks: PlayChunk[];
  /** `true` while the file is still being recorded: ask again for more chunks. */
  open: boolean;
  width: number;
  height: number;
  hasAudio: boolean;
}
