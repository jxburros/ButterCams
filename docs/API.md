# ButterCams REST API (v1)

All endpoints live under `/api/v1` and speak JSON. This document is the contract
between the guardian's HTTP server (`internal/api`) and its clients (the bundled
web UI, and extensions in later versions). Endpoints may be added; existing ones
never change without a new version prefix (spec section 11.3). What ButterCams
v2 (live view and timeline playback, spec section 16) added is marked "v2"; the
prefix stays `/api/v1`.

## Conventions

- **Auth:** a session cookie (`buttercams_session`, HttpOnly, SameSite=Strict,
  Secure when HTTPS is on) set by `POST /auth/login`. Sessions expire after 12 h idle.
- **CSRF:** every `POST`, `PUT` and `DELETE` except `/auth/login` must send the
  header `X-CSRF-Token: <csrfToken>`; the token is returned by login and by
  `GET /auth/session`.
- **Roles:** `admin` (everything) and `viewer` (all `GET` endpoints below except
  `/users` and `/system/config/export`, plus segment downloads, live view,
  playback and `PUT /live/views/mine`).
- **Camera limits (v2):** a Viewer account can be limited to a list of cameras
  (`cameras` in Users). For such a user every list is filtered to those
  cameras, and a request that names another camera is answered `403 forbidden`
  (`404 not_found` when it names a segment, so ids cannot be probed). They see
  only the events and health reasons that belong to their cameras. Admins and
  Viewers without a limit see everything.
- **Timestamps:** RFC 3339 strings. `null` when unknown.
- **Errors:** non-2xx responses carry

  ```json
  {"error": {"code": "validation_failed", "message": "Fix the highlighted fields.",
             "fields": [{"field": "host", "message": "Enter the camera's IP address or host name."}]}}
  ```

  `fields` is present only for `validation_failed` (HTTP 400). Other codes:
  `unauthorized` (401), `invalid_credentials` (401), `account_locked` (423),
  `forbidden` (403), `csrf_failed` (403), `not_found` (404), `conflict` (409),
  `timeout` (503), `internal` (500). The live view and playback routes add
  `media_unavailable` (503: the media process is restarting), `media_busy`
  (503: 200 video connections are already open), `too_many_streams` (503: four
  streams of this camera are already open in this session) and `live_disabled`
  (503: live view is switched off).

## Auth

| Method | Path | Body | Response |
| --- | --- | --- | --- |
| POST | `/auth/login` | `{"username","password"}` | `200 {"user":{"username","role"},"csrfToken"}` |
| POST | `/auth/logout` | – | `204` |
| GET | `/auth/session` | – | `200 {"user":{"username","role"},"csrfToken"}` or `401` |

Five failed logins lock the account for 15 minutes (`423 account_locked`).

## Health

`GET /health` – no auth needed from localhost; otherwise a session is required.

```json
{
  "status": "healthy",            // healthy | warning | critical
  "reasons": [
    {"severity": "warning", "code": "camera_not_recording", "message": "Front Door has not recorded for 3 minutes (backoff: connection refused).", "cameraId": "front-door"}
  ],
  "checkedAt": "2026-11-14T09:00:05.120-06:00",
  "version": "1.0.0",
  "uptimeSec": 86400
}
```

## Cameras

`GET /cameras` → `{"cameras": [Camera]}`

```json
{
  "id": "front-door", "name": "Front Door", "enabled": true,
  "host": "192.168.1.50", "port": 554, "path": "/cam/realmonitor?channel=1&subtype=0",
  "username": "admin", "hasPassword": true,
  "transport": "tcp", "recordAudio": true, "destinationIds": ["nas-1"],
  "subStreamPath": "/cam/realmonitor?channel=1&subtype=1",   // null = no sub stream
  "recordingMode": "continuous", "extensions": {},
  "stats": {
    "state": "recording",          // disabled | connecting | recording | backoff | error
    "stateSince": "2026-11-14T08:00:00.000-06:00",
    "message": "",                 // reason for backoff / error
    "lastPacketTs": "2026-11-14T09:00:04.900-06:00",
    "lastPacketAgeSec": 0.1,       // age of the last video packet when the recorder reported (server clock)
    "reportAgeSec": 2.4,           // seconds since that report (server clock)
    "videoCodec": "h264", "audioCodec": "aac",
    "audioNote": "",               // e.g. "audio not recorded: speex"
    "width": 2688, "height": 1520, "fps": 15,
    "keyframeIntervalMs": 1000, "bitrateKbps": 4096,
    "currentFile": "front-door_2026-11-14_0900_-0600.part.mp4",
    "currentFileBytes": 12345678,
    "singleSession": false,
    "workerId": 1,
    "updatedAt": "2026-11-14T09:00:05.000-06:00"
  }
}
```

`stats` is `null` until the camera's worker has reported.

| Method | Path | Notes |
| --- | --- | --- |
| POST | `/cameras` | Body: CameraInput. `201` → Camera |
| PUT | `/cameras/{id}` | Body: CameraInput (`id` ignored). `200` → Camera |
| DELETE | `/cameras/{id}?keepRecordings=true\|false` | `204`. With `false`, the camera's indexed recordings are deleted everywhere |
| POST | `/cameras/test` | Body: CameraInput (+ optional `id` to reuse the saved password). `200` → CameraTestResult |
| POST | `/cameras/import` | Body: `{"csv": "name,host,port,path,username,password,destinations\n..."}` → `{"created": 2, "results": [{"line": 2, "name": "Lobby", "id": "lobby", "ok": true, "errors": []}]}` |

CameraInput:

```json
{
  "id": "front-door",              // optional on create (generated from name); ignored on update
  "name": "Front Door", "enabled": true,
  "url": "",                       // optional full rtsp:// URL; when set it overrides host/port/path and embedded credentials fill username/password
  "host": "192.168.1.50", "port": 554, "path": "/stream1",
  "username": "admin",
  "password": "secret",            // omit or "" to keep the saved password
  "clearPassword": false,          // true removes the saved password
  "transport": "tcp", "recordAudio": true, "destinationIds": ["nas-1"],
  "subStreamPath": "/stream2"      // v2. Omit to keep the saved value; "" removes the sub stream
}
```

CameraTestResult:

```json
{
  "ok": true, "error": "",
  "videoCodec": "h264", "width": 2688, "height": 1520, "fps": 15, "keyframeIntervalMs": 1000,
  "audioCodec": "pcm_mulaw", "audioNote": "will be converted to AAC",
  "secondSessionAllowed": true,    // null when it could not be determined
  "warnings": ["Set the camera's I-frame interval to 1–2 seconds for clean hourly rollovers."],
  "sub": {                         // v2: the sub stream's result; null when the camera has none
    "ok": true, "error": "", "videoCodec": "h264", "width": 640, "height": 360,
    "warnings": []                 // e.g. "Set the sub stream to H.264 so every browser can show the grid."
  }
}
```

The sub stream is tested after the main stream. Without a sub stream, `warnings`
recommends setting one.

The CSV destinations column is a `;`-separated list of destination ids.

## Destinations

`GET /destinations` →

```json
{
  "destinations": [{
    "id": "nas-1", "name": "NAS main share", "type": "unc", "path": "\\\\nas01\\cameras",
    "username": "CORP\\svc-buttercams", "hasPassword": true,
    "capGB": 20000, "maxAgeDays": null, "enabled": true,
    "status": {
      "state": "ok",               // ok | retrying | disabled
      "lastError": "", "queueLength": 0, "oldestQueuedTs": null,
      "usedBytes": 123456789, "freeBytes": 98765432100, "totalBytes": 200000000000,
      "lastSuccessTs": "2026-11-14T09:01:10.000-06:00"
    }
  }],
  "staging": {
    "path": "D:\\ButterCams\\staging", "minFreePercent": 10, "minFreeGB": 50,
    "freeBytes": 500000000000, "totalBytes": 1000000000000, "floorBytes": 100000000000,
    "usedBytes": 4000000000
  }
}
```

`freeBytes`/`totalBytes` are `null` until first measured.

| Method | Path | Notes |
| --- | --- | --- |
| POST | `/destinations` | Body: DestinationInput. `201` → Destination |
| PUT | `/destinations/{id}` | `200` → Destination |
| DELETE | `/destinations/{id}` | `204`; `409` while cameras still use it |
| POST | `/destinations/test` | Body: DestinationInput (+ optional `id` to reuse the saved password) → `{"ok": true, "error": "", "freeBytes": 1, "totalBytes": 2, "durationMs": 120}` |

DestinationInput: `{"id"?, "name", "type": "local"|"unc", "path", "username", "password"?, "clearPassword"?, "capGB", "maxAgeDays": number|null, "enabled"}`.

## Recordings

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/recordings/cameras` | `{"cameras": [{"id","name","deleted": false}]}` – every camera that has indexed recordings or is configured |
| GET | `/segments?camera=<id>&date=YYYY-MM-DD` | `{"segments": [Segment]}` ordered by start |
| GET | `/segments/{id}/download` | The MP4 file from the nearest available copy (`404` when no copy is reachable) |

Segment:

```json
{
  "id": 42, "cameraId": "front-door", "fileName": "front-door_2026-11-14_0900_-0600.mp4",
  "start": "2026-11-14T09:00:00.212-06:00", "end": "2026-11-14T10:00:00.180-06:00",
  "bytes": 1811939328, "videoCodec": "h264", "audioCodec": "aac",
  "gapBefore": false, "closeReason": "hour_rollover", "protected": false,
  "copies": [{"destinationId": "staging", "state": "deleted", "lastError": "", "updatedTs": "…"},
             {"destinationId": "nas-1", "state": "verified", "lastError": "", "updatedTs": "…"}]
}
```

Copy states: `queued`, `copying`, `verified`, `failed`, `deleted`. `end` and
`bytes` are `null` for the file currently being written.

## Events

`GET /events?since=<RFC3339>&severity=info|warning|critical&camera=<id>&limit=200&before=<id>` →
`{"events": [{"id": 9, "ts": "…", "source": "camera:front-door", "type": "stream_stalled", "severity": "warning", "cameraId": "front-door", "data": {"message": "…"}}]}`
newest first. `limit` defaults to 200 (max 1000). `before` pages backwards by id.

## System

`GET /system` →

```json
{
  "version": "1.0.0", "startedAt": "…", "uptimeSec": 3600, "hostname": "NVR01", "os": "windows/amd64",
  "dataRoot": "C:\\ProgramData\\ButterCams", "schemaVersion": 2,
  "server": {"listenAddress": "0.0.0.0", "port": 8650, "https": true, "allowedSubnets": []},
  "staging": {"path": "D:\\ButterCams\\staging", "minFreePercent": 10, "minFreeGB": 50, "freeBytes": 1, "totalBytes": 2},
  "logging": {"retentionDays": 14},
  "certificate": {"subject": "CN=NVR01", "notAfter": "…", "selfSigned": true, "dnsNames": ["NVR01"], "fingerprintSha256": "AB:CD:…"},
  "counts": {"cameras": 60, "enabledCameras": 58, "destinations": 2, "workers": 10},
  "rebuild": {"running": false, "startedAt": null, "finishedAt": null, "error": "", "segments": 0},
  "live": {"enabled": true, "mainStream": true},
  "media": {                       // the media process (live view and playback)
    "enabled": true, "state": "running",   // running | starting | off
    "viewers": 3, "overflows": 0, "memBytes": 52428800,
    "sources": [{"cameraId": "front-door", "stream": "sub", "viewers": 2, "state": "playing"}]
  }
}
```

`media.sources` is empty for Viewers with a camera limit.

| Method | Path | Notes |
| --- | --- | --- |
| PUT | `/system` | Body: any of `{"server": {...}, "staging": {"path","minFreePercent","minFreeGB"}, "logging": {"retentionDays"}, "live": {"enabled","mainStream"}}` → same shape as `GET /system`. Changing the address, port or HTTPS restarts the listener about a second after the response |
| POST | `/system/rebuild-index` | `202 {"started": true}`; progress under `rebuild` in `GET /system` |
| POST | `/system/certificate` | Body: `{"certPem","keyPem"}` or `{"pfxBase64","password"}` → `200 {"certificate": {...}}` |
| GET | `/system/config/export` | `config.json` with every password removed (admin only) |
| POST | `/system/config/import` | Body: a config document. Passwords are kept for cameras/destinations whose `id` already exists |

## Users (admin only)

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/users` | `{"users": [{"username","role","createdAt","locked": false,"cameras": null}]}` |
| POST | `/users` | `{"username","password","role","cameras"?}` → `201` |
| PUT | `/users/{username}` | `{"password"?, "role"?, "cameras"?}` → `200` |
| DELETE | `/users/{username}` | `204`; the last admin cannot be deleted |

Passwords need at least 8 characters.

`cameras` (v2) limits a Viewer to a list of camera ids. `null` means every
camera; an empty list means none. On `PUT`, leaving `cameras` out keeps the
current limit and `null` removes it. The limit is ignored for Admins.

## Event stream

`GET /stream` – Server-Sent Events. Event names and payloads:

| Event | Data |
| --- | --- |
| `health` | Same object as `GET /health`; sent on connect and on every change |
| `cameras` | `{"cameras": [{"id": "front-door", "stats": {...}}]}` every 5 s |
| `destinations` | Same object as `GET /destinations` every 5 s |
| `event` | One row of the events table as it is written |

For a Viewer with a camera limit, `cameras` holds only their cameras, `health`
only the reasons of their cameras, and `event` only events of their cameras.

## Live view (v2)

`GET /live/{camera}/{main|sub}?audio=0|1` – a WebSocket. `sub` is the camera's
sub stream, `main` the stream that is being recorded. The server sends:

1. a text message `{"type":"init","mime":"video/mp4; codecs=\"avc1.640028\"","width":2688,"height":1520}`;
   `mime` is ready for `MediaSource.addSourceBuffer`;
2. a binary message with the MP4 initialization segment;
3. binary messages with fragments, one per video frame, starting at a keyframe.

A new `init` message and initialization segment follow whenever the stream's
parameters change. While there is no video the server sends
`{"type":"state","state":"waiting","message":"…"}` and keeps the socket open; the
picture starts by itself when the camera is back. The client sends nothing.
Audio is included only with `audio=1`. The handshake is refused when its
`Origin` is another site. A viewer that reads too slowly loses frames and
resumes at the next keyframe; it never delays anyone else.

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/live/views` | `{"shared": [View], "mine": [View]}` |
| PUT | `/live/views/mine` | Body: `{"views": [View]}`. Replaces the signed-in user's own views (at most 50). Every role |
| PUT | `/live/views/shared` | Body: `{"views": [View]}`. Replaces the shared views (admin only) |

View: `{"id": "view-entrances", "name": "Entrances", "layout": 4, "cameraIds": ["front-door", "loading-dock"]}`.
`layout` is 1, 4, 9 or 16. Both `PUT`s answer like `GET /live/views`. A view
shows each user only the cameras they may see; cameras that do not exist are
dropped when a view is saved.

## Timeline and playback (v2)

`GET /timeline?camera=<id>&from=<RFC3339>&to=<RFC3339>` – the recorded spans of
one camera that overlap `[from, to)` and its recorder events in that range, from
the index. At most 31 days per request.

```json
{
  "camera": "front-door", "from": "…", "to": "…", "now": "2026-11-14T10:20:00.000-06:00",
  "segments": [
    {"id": 42, "start": "2026-11-14T09:00:00.212-06:00", "end": "2026-11-14T10:00:00.180-06:00",
     "gapBefore": false, "videoCodec": "h264", "audioCodec": "aac", "open": false,
     "fileName": "front-door_2026-11-14_0900_-0600.mp4", "bytes": 1811939328},
    {"id": 43, "start": "2026-11-14T10:00:00.180-06:00", "end": null,
     "gapBefore": false, "videoCodec": "h264", "audioCodec": "aac", "open": true,
     "fileName": "front-door_2026-11-14_1000_-0600.mp4", "bytes": null}
  ],
  "events": [{"id": 9, "ts": "…", "source": "camera:front-door", "type": "stream_stalled", "severity": "warning", "cameraId": "front-door", "data": {"message": "…"}}]
}
```

`open` marks the file that is being recorded. `events` are oldest first and
hold only recorder events (stalls, reconnects, clock changes).

The browser builds playback itself from these four routes; the server keeps no
playback state. All of them read the nearest available copy of the file
(`404` when none is reachable).

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/segments/{id}/play/index` | The playback index of one file (below) |
| GET | `/segments/{id}/play/data` | The file's bytes. A `Range` header is required (`400 range_required` without). For the file being recorded, a range past its last complete fragment gets `416` |
| GET | `/segments/{id}/play/keyframes?from=<ms>&to=<ms>` | Fragments that hold only the first frame (a keyframe) of each chunk in the range, each lasting as long as its chunk; at most 120 per request. They carry video only: play them in a source buffer of their own made with `videoMime`, and add `init=1` to get a video-only initialization segment in front. Times are milliseconds from the start of the file |
| GET | `/segments/{id}/play/thumb?t=<ms>` | A complete small MP4 with the one keyframe at or before `t` |

Playback index:

```json
{
  "mime": "video/mp4; codecs=\"avc1.640028, mp4a.40.2\"",
  "videoMime": "video/mp4; codecs=\"avc1.640028\"",   // the video track alone, for keyframe mode
  "init": [0, 1532],               // byte offset and length of the initialization segment
  "start": "2026-11-14T09:00:00.212-06:00",
  "durationMs": 3599968,
  "chunks": [[0, 1000, 1532, 418200], [1000, 1000, 419732, 402112]],   // [startMs, durationMs, offset, size]
  "open": false,                   // true for the file being recorded: ask again for more chunks
  "width": 2688, "height": 1520, "hasAudio": true
}
```

Times are milliseconds with three decimals, so `round(ms * 90)` is the exact
90 kHz tick. `start` is `null` for a file that does not carry its start time.
A chunk starts at a keyframe and can be fetched and appended on its own. Media
times inside a file start at zero; the player places each file on its own
timeline (`timestampOffset`).
