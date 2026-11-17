// The one place that talks to the ButterCams REST API (docs/API.md).

import type {
  Camera,
  CameraImportRequest,
  CameraImportResponse,
  CameraInput,
  CameraTestResult,
  CamerasResponse,
  CertificateInfo,
  CertificateResponse,
  CertificateUpload,
  Destination,
  DestinationInput,
  DestinationTestResult,
  DestinationsResponse,
  EventsQuery,
  EventsResponse,
  FieldError,
  Health,
  LiveView,
  LiveViewsResponse,
  LoginRequest,
  PlayIndex,
  RebuildStarted,
  RecordingCamerasResponse,
  SegmentsResponse,
  Session,
  StreamKind,
  SystemInfo,
  SystemUpdate,
  TimelineResponse,
  UserCreate,
  UserUpdate,
  UsersResponse,
} from './types';

const BASE = '/api/v1';

/**
 * A failed API call. `code` is one of the codes in docs/API.md, or `network`
 * (the server could not be reached) or `bad_response` (the reply was not the
 * documented JSON).
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly fields: FieldError[];

  constructor(status: number, code: string, message: string, fields: FieldError[] = []) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.fields = fields;
  }
}

/** The message to show for any thrown value. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error && err.message) return err.message;
  return 'Something went wrong. Try again.';
}

let csrfToken = '';
let unauthorizedHandler: (() => void) | null = null;

/** Called by the session context whenever the token changes. */
export function setCsrfToken(token: string): void {
  csrfToken = token;
}

/** Registers what happens when the server answers 401 (the session is gone). */
export function setUnauthorizedHandler(handler: (() => void) | null): void {
  unauthorizedHandler = handler;
}

type Method = 'GET' | 'POST' | 'PUT' | 'DELETE';

interface RequestOptions {
  body?: unknown;
  /** Login reports wrong credentials with 401; that must not count as a lost session. */
  isLogin?: boolean;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function parseFields(value: unknown): FieldError[] {
  if (!Array.isArray(value)) return [];
  const fields: FieldError[] = [];
  for (const item of value) {
    if (isRecord(item) && typeof item.field === 'string' && typeof item.message === 'string') {
      fields.push({ field: item.field, message: item.message });
    }
  }
  return fields;
}

function fallbackMessage(status: number): string {
  switch (status) {
    case 401:
      return 'Your session has ended. Sign in again.';
    case 403:
      return 'Your account is not allowed to do that.';
    case 404:
      return 'That item no longer exists.';
    case 503:
      return 'The server took too long to answer. Try again.';
    default:
      return `The server answered with an unexpected error (HTTP ${status}).`;
  }
}

async function toApiError(res: Response): Promise<ApiError> {
  let parsed: unknown = null;
  try {
    parsed = JSON.parse(await res.text());
  } catch {
    parsed = null;
  }
  if (isRecord(parsed) && isRecord(parsed.error)) {
    const { code, message, fields } = parsed.error;
    return new ApiError(
      res.status,
      typeof code === 'string' && code !== '' ? code : 'internal',
      typeof message === 'string' && message !== '' ? message : fallbackMessage(res.status),
      parseFields(fields),
    );
  }
  return new ApiError(res.status, 'bad_response', fallbackMessage(res.status));
}

async function send(method: Method, path: string, options: RequestOptions = {}): Promise<Response> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  const init: RequestInit = { method, headers, credentials: 'same-origin', cache: 'no-store' };
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(options.body);
  }
  if (method !== 'GET' && !options.isLogin) {
    headers['X-CSRF-Token'] = csrfToken;
  }

  let res: Response;
  try {
    res = await fetch(BASE + path, init);
  } catch {
    throw new ApiError(
      0,
      'network',
      'Could not reach the ButterCams server. Check that the service is running and that this PC can reach it.',
    );
  }
  if (res.ok) return res;

  const err = await toApiError(res);
  if (res.status === 401 && !options.isLogin) {
    unauthorizedHandler?.();
  }
  throw err;
}

async function request<T>(method: Method, path: string, options: RequestOptions = {}): Promise<T> {
  const res = await send(method, path, options);
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (text.trim() === '') return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError(res.status, 'bad_response', 'The server sent a reply this page could not read.');
  }
}

function query(params: Record<string, string | number | boolean | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') search.set(key, String(value));
  }
  const text = search.toString();
  return text === '' ? '' : `?${text}`;
}

const seg = encodeURIComponent;

/** A Go server encodes a list it never filled as `null`; read that as an empty list. */
function list<T>(value: T[] | null | undefined): T[] {
  return value ?? [];
}

function tidyCamera(camera: Camera): Camera {
  return { ...camera, destinationIds: list(camera.destinationIds) };
}

function tidyCertificate(certificate: CertificateInfo | null): CertificateInfo | null {
  return certificate ? { ...certificate, dnsNames: list(certificate.dnsNames) } : null;
}

function tidySystem(system: SystemInfo): SystemInfo {
  return {
    ...system,
    server: { ...system.server, allowedSubnets: list(system.server.allowedSubnets) },
    certificate: tidyCertificate(system.certificate),
    media: { ...system.media, sources: list(system.media?.sources) },
  };
}

function tidyViews(response: LiveViewsResponse): LiveViewsResponse {
  const tidy = (views: LiveView[] | null) => list(views).map((view) => ({ ...view, cameraIds: list(view.cameraIds) }));
  return { shared: tidy(response.shared), mine: tidy(response.mine) };
}

export const api = {
  // Auth
  login: (body: LoginRequest) => request<Session>('POST', '/auth/login', { body, isLogin: true }),
  logout: () => request<void>('POST', '/auth/logout'),
  session: () => request<Session>('GET', '/auth/session'),

  // Health
  health: async (): Promise<Health> => {
    const health = await request<Health>('GET', '/health');
    return { ...health, reasons: list(health.reasons) };
  },

  // Cameras
  cameras: async (): Promise<CamerasResponse> => {
    const response = await request<CamerasResponse>('GET', '/cameras');
    return { cameras: list(response.cameras).map(tidyCamera) };
  },
  createCamera: (body: CameraInput) => request<Camera>('POST', '/cameras', { body }),
  updateCamera: (id: string, body: CameraInput) => request<Camera>('PUT', `/cameras/${seg(id)}`, { body }),
  deleteCamera: (id: string, keepRecordings: boolean) =>
    request<void>('DELETE', `/cameras/${seg(id)}${query({ keepRecordings })}`),
  testCamera: async (body: CameraInput): Promise<CameraTestResult> => {
    const result = await request<CameraTestResult>('POST', '/cameras/test', { body });
    return {
      ...result,
      warnings: list(result.warnings),
      sub: result.sub ? { ...result.sub, warnings: list(result.sub.warnings) } : null,
    };
  },
  importCameras: async (body: CameraImportRequest): Promise<CameraImportResponse> => {
    const response = await request<CameraImportResponse>('POST', '/cameras/import', { body });
    return {
      ...response,
      results: list(response.results).map((line) => ({ ...line, errors: list(line.errors) })),
    };
  },

  // Destinations
  destinations: async (): Promise<DestinationsResponse> => {
    const response = await request<DestinationsResponse>('GET', '/destinations');
    return { ...response, destinations: list(response.destinations) };
  },
  createDestination: (body: DestinationInput) => request<Destination>('POST', '/destinations', { body }),
  updateDestination: (id: string, body: DestinationInput) =>
    request<Destination>('PUT', `/destinations/${seg(id)}`, { body }),
  deleteDestination: (id: string) => request<void>('DELETE', `/destinations/${seg(id)}`),
  testDestination: (body: DestinationInput) =>
    request<DestinationTestResult>('POST', '/destinations/test', { body }),

  // Recordings
  recordingCameras: async (): Promise<RecordingCamerasResponse> => {
    const response = await request<RecordingCamerasResponse>('GET', '/recordings/cameras');
    return { cameras: list(response.cameras) };
  },
  segments: async (camera: string, date: string): Promise<SegmentsResponse> => {
    const response = await request<SegmentsResponse>('GET', `/segments${query({ camera, date })}`);
    return { segments: list(response.segments).map((segment) => ({ ...segment, copies: list(segment.copies) })) };
  },
  /** For a plain `<a href download>`; the session cookie authorises it. */
  segmentDownloadUrl: (id: number) => `${BASE}/segments/${id}/download`,

  // Timeline and playback (spec 16.5)
  timeline: async (camera: string, from: Date, to: Date): Promise<TimelineResponse> => {
    const response = await request<TimelineResponse>(
      'GET',
      `/timeline${query({ camera, from: from.toISOString(), to: to.toISOString() })}`,
    );
    return { ...response, segments: list(response.segments), events: list(response.events) };
  },
  playIndex: async (id: number): Promise<PlayIndex> => {
    const index = await request<PlayIndex>('GET', `/segments/${id}/play/index`);
    return { ...index, chunks: list(index.chunks) };
  },
  /** Fetch with a `Range: bytes=a-b` header; the session cookie authorises it. */
  playDataUrl: (id: number) => `${BASE}/segments/${id}/play/data`,
  /** Keyframe-only fragments between two times (milliseconds from the start of the file). */
  playKeyframesUrl: (id: number, fromMs: number, toMs: number) =>
    `${BASE}/segments/${id}/play/keyframes${query({ from: Math.floor(fromMs), to: Math.ceil(toMs) })}`,
  /** A complete small MP4 holding the keyframe at or before a time; usable as `<video src>`. */
  playThumbUrl: (id: number, tMs: number) => `${BASE}/segments/${id}/play/thumb${query({ t: Math.floor(tMs) })}`,

  // Live view (spec 16.4)
  /** WebSocket URL of a camera's live stream. */
  liveUrl: (camera: string, stream: StreamKind, audio: boolean): string => {
    const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${scheme}//${window.location.host}${BASE}/live/${seg(camera)}/${stream}?audio=${audio ? 1 : 0}`;
  },
  liveViews: async (): Promise<LiveViewsResponse> => tidyViews(await request<LiveViewsResponse>('GET', '/live/views')),
  saveMyViews: async (views: LiveView[]): Promise<LiveViewsResponse> =>
    tidyViews(await request<LiveViewsResponse>('PUT', '/live/views/mine', { body: { views } })),
  saveSharedViews: async (views: LiveView[]): Promise<LiveViewsResponse> =>
    tidyViews(await request<LiveViewsResponse>('PUT', '/live/views/shared', { body: { views } })),

  // Events
  events: async (params: EventsQuery): Promise<EventsResponse> => {
    const response = await request<EventsResponse>('GET', `/events${query({ ...params })}`);
    return { events: list(response.events) };
  },

  // System
  system: async (): Promise<SystemInfo> => tidySystem(await request<SystemInfo>('GET', '/system')),
  updateSystem: async (body: SystemUpdate): Promise<SystemInfo> =>
    tidySystem(await request<SystemInfo>('PUT', '/system', { body })),
  rebuildIndex: () => request<RebuildStarted>('POST', '/system/rebuild-index'),
  uploadCertificate: async (body: CertificateUpload): Promise<CertificateResponse> => {
    const response = await request<CertificateResponse>('POST', '/system/certificate', { body });
    return { certificate: { ...response.certificate, dnsNames: list(response.certificate.dnsNames) } };
  },
  /** The exported config.json (passwords removed) as a file to save. */
  exportConfig: async (): Promise<Blob> => (await send('GET', '/system/config/export')).blob(),
  importConfig: (config: unknown) => request<unknown>('POST', '/system/config/import', { body: config }),

  // Users
  users: async (): Promise<UsersResponse> => {
    const response = await request<UsersResponse>('GET', '/users');
    return { users: list(response.users) };
  },
  createUser: (body: UserCreate) => request<unknown>('POST', '/users', { body }),
  updateUser: (username: string, body: UserUpdate) =>
    request<unknown>('PUT', `/users/${seg(username)}`, { body }),
  deleteUser: (username: string) => request<void>('DELETE', `/users/${seg(username)}`),
};
