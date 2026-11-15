# ButterCams — Build Specification

> **For the implementing agent:** This file is the source of truth for what ButterCams is and how each part must work. It is divided into four versions:
>
> | Version | Name | Sections |
> | --- | --- | --- |
> | v1 | Core Recorder | 0–15 |
> | v2 | Live View and Timeline Playback | 16 |
> | v3 | Alerts, Clip Export and Motion Detection | 17 |
> | v4 | AI Object Detection | 18 |
>
> **Status:** v1 is built. **v2 is the next phase of development**; `docs/V2_PLAN.md` describes how to build it. (Live view and timeline playback were originally planned last, as v4. They were moved forward, and redesigned so that they need nothing from the later versions; see `docs/DECISIONS.md`, decisions 35–43.)
>
> Sections 0, 1, 3, 19 and 20 apply to every version; Section 2 lists the v1 requirements. Each version builds on the ones before it and must keep every rule in Section 1. If something here is ambiguous, choose the option that best protects continuous recording, and record the decision in `docs/DECISIONS.md`.

---

## 0. Summary

ButterCams is a native Windows Server background service that records many RTSP IP cameras (target: 60+) continuously into **one-hour MP4 files** and copies them to storage destinations the operator chooses. It deletes the oldest files when a destination reaches its size limit. It monitors itself and recovers from every failure automatically. It is configured through a web settings page that other PCs on the network can reach.

**v1 has one job: record every camera faithfully and never stop.** It does no motion detection, no AI, no live view and no transcoding of video.

- **Product name:** ButterCams ("runs smooth as butter")
- **First user / tester:** Garrett
- **Target OS:** Windows Server 2019, 2022 and 2025 (x64). It must also run on Windows 10/11 x64 for development.
- **Form:** a Windows service plus a web settings page. No desktop GUI.

---

## 1. Non-negotiable rules

These rules override convenience. Any code that breaks one is a bug.

1. **Recording comes first.** Nothing (UI, API, copying, retention, logging, future extensions) may block, slow or crash the recording path.
2. **Never decode or re-encode video.** Video packets from the camera are written to file as-is, and sent to viewers as-is (Section 16). The only allowed transcoding is *audio* from G.711/G.726 to AAC (Section 6.3), because it is tiny and cheap.
3. **Isolate failures.** A misbehaving camera, a full disk or an offline share only affects its own part. One camera can never take down another.
4. **Self-heal, then report.** Every failure in Section 9 has an automatic recovery and a visible status. Never require a human to restart anything.
5. **Open formats.** Every recording plays in VLC and Windows Media Player with no ButterCams software.
6. **No silent failure.** If recording isn't happening for an enabled camera, the status page and logs must say so within 30 seconds.
7. **Delete only what we wrote.** Retention and cleanup touch only files listed in ButterCams' index, never other files in a folder.
8. **Minimal surface.** Do not add features not in this spec. Put ideas in `docs/IDEAS.md`.

---

## 2. Requirements from the user (v1)

| # | Requirement | Spec section |
| --- | --- | --- |
| R1 | Record each camera's RTSP feed given its URL, credentials and port | 6 |
| R2 | Write recordings as one-hour clips (fixed, not configurable in v1) | 6.4 |
| R3 | Recordings go to destinations the operator chooses: local disk, attached storage, SMB/file share | 7 |
| R4 | The operator is prompted for, and can later change, where data is stored | 7.1, 11 |
| R5 | Set a storage limit per destination; delete the oldest files first when full | 8 |
| R6 | Refresh each camera's stream every hour so it always has a fresh feed | 6.5 |
| R7 | Open, efficient format (MP4), low CPU | 6.2 |
| R8 | Support about 60 cameras with the CPU nearly idle | 12 |
| R9 | Record audio when a camera sends it | 6.3 |
| R10 | Never die: self-check, self-recover, always stay on | 9, 10 |
| R11 | Settings page reachable from other PCs on the network | 11 |
| R12 | Run on Windows Server | 0, 13 |

---

## 3. Technology stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Core language | **Go** (latest stable) | One binary: `buttercams.exe` |
| RTSP client | `github.com/bluenviron/gortsplib` (latest major version) | TCP transport by default, UDP optional per camera |
| MP4 writing | `github.com/bluenviron/mediacommon` fMP4 package (latest major version) | Fragmented MP4 writer |
| Windows service | `golang.org/x/sys/windows/svc` and `.../svc/mgr`, `.../svc/eventlog` | |
| Database | SQLite via `modernc.org/sqlite` (pure Go, no CGO) | WAL mode |
| Audio transcode | Pure-Go G.711 decoder plus an AAC encoder. If no reliable pure-Go AAC encoder exists, use a bundled `ffmpeg.exe` **for audio only**, run as a supervised side process per affected camera | Record the choice in `docs/DECISIONS.md` |
| Web UI | TypeScript, React, Vite | Built to static files and embedded in the Go binary with `embed` |
| Logging | Go `log/slog`, JSON output, rotating files | |
| Live view and playback (v2) | Fragmented MP4 to the browser's Media Source Extensions: over a WebSocket for live, HTTP range requests for playback | No video decoding on the server, no extra ports, no bundled media server (Section 16) |
| Installer | WiX v4 MSI (preferred) or Inno Setup | Section 13 |
| Password hashing | `golang.org/x/crypto/argon2` (argon2id) | |
| Secret storage | Windows DPAPI (`CryptProtectData`, machine scope) | Camera and share passwords |

**Do not use:** CGO, Electron, any cloud service, telemetry, or any runtime that must be installed separately (except optional ffmpeg for audio, bundled by the installer).

---

## 4. Architecture

One executable, three process roles, chosen by a sub-command:

```
buttercams.exe service      # Guardian: the Windows service (run by the SCM)
buttercams.exe worker --id N  # Recorder worker (spawned by the guardian)
buttercams.exe mover        # Copy + retention process (spawned by the guardian)
buttercams.exe console      # Run the guardian in the foreground for development
buttercams.exe rebuild-index  # Rebuild the SQLite index from files on disk
buttercams.exe version
```

```
            ┌──────────────────────────────────────────────────────────┐
 Browser ──►│ Guardian (Windows service)                               │
 (any PC)   │  • starts / watches / restarts workers and the mover     │
            │  • owns config.json and the SQLite index                 │
            │  • serves web UI + REST API (HTTPS)                      │
            │  • runs the health check every 60 s                      │
            └───────┬──────────────────────────────────┬───────────────┘
                    │ supervises (Job Object + pipes)  │ supervises
                    ▼                                  ▼
 Cameras ──RTSP──► Recorder workers ──fMP4──► Staging ──► Mover ──► Destinations
 (60+)            (≤ 6 cameras each)         (local disk)  (copy,    (local, attached,
                                                            verify,   UNC share)
                                                            retention)
```

### 4.1 Guardian (`service`)

- Registered as Windows service `ButterCams`, display name "ButterCams Recorder", start type **Automatic (Delayed Start)**.
- Creates a **Windows Job Object** with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` and puts every child process in it, so children never outlive the guardian.
- Assigns cameras to workers: up to 6 cameras per worker (constant `CamerasPerWorker = 6`), filling workers in camera order. Changing assignments must only restart the affected worker.
- Owns the only write handle to `config.json` and `buttercams.db`. Workers and the mover talk to the guardian; they never open the database themselves.
- Serves the web UI and API (Section 11).
- Runs the health check (Section 10).
- On shutdown (service stop or OS shutdown): tells every worker to finalize files, waits up to 15 s, then exits. Must handle `SERVICE_CONTROL_PRESHUTDOWN`.

### 4.2 Recorder worker (`worker`)

- Receives its camera list from the guardian over stdin (JSON lines, Section 4.4).
- Each camera runs in its own goroutine with its own state machine (Section 6.1). A panic in one camera's goroutine is recovered, logged and treated as a stream failure for that camera only.
- Writes only to the staging folder.
- Sends a heartbeat every 5 s and per-camera stats every 5 s over stdout.
- Exits cleanly when stdin closes (guardian gone) after finalizing files.

### 4.3 Mover (`mover`)

- Receives "segment closed" jobs from the guardian.
- Copies each closed file to each of its camera's destinations, verifies it, reports results, and runs retention (Section 8).
- Has a bounded worker pool (default 4 concurrent copies) so it never saturates the disk the recorders write to.

### 4.4 Inter-process messages

JSON lines over the child's stdin/stdout. Stderr is captured into the guardian's log. Every message has `type` and `ts` (RFC 3339 with milliseconds).

Guardian → worker:

```json
{"type":"assign","ts":"…","cameras":[{ /* camera config, Section 5 */ }]}
{"type":"update_camera","ts":"…","camera":{ /* … */ }}
{"type":"remove_camera","ts":"…","cameraId":"front-door"}
{"type":"shutdown","ts":"…"}
```

Worker → guardian:

```json
{"type":"heartbeat","ts":"…","workerId":3,"memBytes":48211968}
{"type":"camera_stats","ts":"…","cameraId":"front-door","state":"recording","lastPacketTs":"…","videoCodec":"h264","audioCodec":"aac","width":2688,"height":1520,"fps":15,"keyframeIntervalMs":1000,"bitrateKbps":4096,"currentFile":"…","currentFileBytes":12345678}
{"type":"segment_opened","ts":"…","cameraId":"…","path":"…","start":"…"}
{"type":"segment_closed","ts":"…","cameraId":"…","path":"…","start":"…","end":"…","bytes":1811939328,"sha256":"…","gap":false,"reason":"hour_rollover"}
{"type":"event","ts":"…","cameraId":"…","severity":"warning","code":"stream_stalled","message":"…"}
```

Guardian ↔ mover uses the same format with `copy_job`, `copy_result`, `retention_result`, `heartbeat`.

Hash the file while writing (streaming SHA-256), so no file is read twice.

---

## 5. Configuration

### 5.1 Location

- Program files: `C:\Program Files\ButterCams\`
- Data root: chosen at install (Section 13), default `C:\ProgramData\ButterCams\`. It holds `config.json`, `buttercams.db`, `logs\`, `certs\`, and by default `staging\`.
- The data root path itself is stored in the registry at `HKLM\SOFTWARE\ButterCams\DataRoot`.

### 5.2 `config.json` schema (v1)

```json
{
  "schemaVersion": 1,
  "server": {
    "listenAddress": "0.0.0.0",
    "port": 8650,
    "https": true,
    "allowedSubnets": []
  },
  "staging": {
    "path": "D:\\ButterCams\\staging",
    "minFreePercent": 10,
    "minFreeGB": 50
  },
  "destinations": [
    {
      "id": "nas-1",
      "name": "NAS main share",
      "type": "unc",
      "path": "\\\\nas01\\cameras",
      "username": "CORP\\svc-buttercams",
      "passwordEnc": "<DPAPI base64>",
      "capGB": 20000,
      "maxAgeDays": null,
      "enabled": true
    },
    {
      "id": "local-d",
      "name": "Local D drive",
      "type": "local",
      "path": "E:\\Recordings",
      "capGB": 4000,
      "maxAgeDays": 30,
      "enabled": true
    }
  ],
  "cameras": [
    {
      "id": "front-door",
      "name": "Front Door",
      "enabled": true,
      "host": "192.168.1.50",
      "port": 554,
      "path": "/cam/realmonitor?channel=1&subtype=0",
      "username": "admin",
      "passwordEnc": "<DPAPI base64>",
      "transport": "tcp",
      "recordAudio": true,
      "destinationIds": ["nas-1"],
      "subStreamPath": null,
      "recordingMode": "continuous",
      "extensions": {}
    }
  ]
}
```

Field rules:

- `id`: lowercase letters, digits and hyphens, 1–48 characters, unique, **cannot change after creation** (it names folders). Generated from `name`; editable only at creation.
- `type`: `local` (fixed or attached drive path) or `unc` (`\\server\share\...`). **Reject mapped drive letters** (any drive letter whose type is `DRIVE_REMOTE`), with the message: "Mapped drives aren't visible to Windows services. Use the \\\\server\\share path instead."
- The full RTSP URL is built as `rtsp://<host>:<port><path>`. The UI also accepts a full pasted URL and splits it into fields, extracting embedded credentials.
- `transport`: `tcp` (default) or `udp`.
- **Reserved for later versions; save and load them but do not use them in v1:** `subStreamPath` (used from v2), `recordingMode` (v1 and v2 accept only `"continuous"`), `extensions`.
- `allowedSubnets`: optional CIDR list. Empty means any address can reach the UI (still behind login).
- One-hour segment length is a **constant** (`SegmentDuration = time.Hour`), not a config field.

### 5.3 Writing config

- Validate fully before saving. Reject invalid config with field-level errors.
- Write to `config.json.tmp`, flush, then atomically replace (`MoveFileEx` with `MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH`).
- Keep the previous 10 versions as `config.json.1` … `config.json.10`.
- Apply changes live: compute the diff and restart only the cameras or destinations that changed.
- Startup with an unreadable `config.json`: load the newest readable backup, raise a Critical status, and log it.
- Include a migration framework (`migrations/config_v1_to_v2.go` pattern), even though v1 has no migrations.

---

## 6. Recording

### 6.1 Camera state machine

```
disabled ──enable──► connecting ──first keyframe──► recording
                         ▲   │                          │
                         │   └─fail──► backoff ◄─stall/error─┘
                         └────────── retry ───────┘
```

- **connecting:** RTSP DESCRIBE/SETUP/PLAY. Timeout 10 s.
- **recording:** a file is open; packets are written.
- **backoff:** wait, then retry. Delays: 2, 4, 8, 16, 32, 60 s, then 60 s forever, with ±20% jitter. Reset to 2 s after 5 minutes of stable recording.
- **Stall detection:** no video packet for **10 s** means a stall. Close the current file cleanly, mark the gap, go to backoff.
- States reported to the guardian: `disabled`, `connecting`, `recording`, `backoff`, `error` (config problem, such as authentication failure: retry every 60 s and show the reason).

### 6.2 Container and video

- **Container:** fragmented MP4 (`ftyp` + `moov` with no samples, then `moof`/`mdat` fragments).
- **Fragment boundary:** at every video keyframe. Also force a fragment if 4 s pass without a keyframe.
- **Flush:** each fragment is written and `FlushFileBuffers` is called at most every 2 s, so a power loss loses at most about 2 s.
- **Video codecs:** H.264 and H.265, copied as received. Take parameter sets (SPS/PPS/VPS) from the SDP and from in-band NAL units. If parameter sets change mid-stream (resolution change), close the file and start a new one.
- **Unsupported video codec** (MJPEG, MPEG-4 Part 2): camera state `error` with the message "Camera sends <codec>; ButterCams v1 records H.264 or H.265 only. Change the camera's encoding setting."
- **Timestamps:** use RTP timestamps for sample timing. Handle RTP timestamp wraparound. Store each file's wall-clock start in the index and in an MP4 `udta` metadata box (`buttercams:start`, `buttercams:camera`).

### 6.3 Audio

- **AAC:** copy as received.
- **G.711 µ-law / A-law, G.726:** transcode to AAC-LC mono, 16 kHz, 32 kbps (or a sample rate matching the source), because MP4 players handle G.711 inconsistently. This is the only transcoding ButterCams does.
- **Opus:** copy as received (MP4 supports Opus).
- **Anything else:** record video only, log a warning once per connection, and show "audio not recorded: <codec>" on the camera's status.
- Audio errors must never stop video recording. On any audio failure, drop audio for the rest of that file and continue.
- `recordAudio: false` skips audio entirely.

### 6.4 Files and naming

- **Segment length:** one hour, aligned to the top of the local-time hour.
- **Path:** `<staging>\<camera-id>\<YYYY-MM-DD>\<camera-id>_<YYYY-MM-DD>_<HH>00_<±HHMM>.mp4`
  - Example: `D:\ButterCams\staging\front-door\2026-11-14\front-door_2026-11-14_0900_-0600.mp4`
  - The UTC offset keeps the repeated hour unique when clocks fall back.
- **Partial files:** if recording restarts within the same hour (stall, crash, restart), the new file gets a suffix: `…_0900_-0600_b.mp4`, then `_c`, and so on. The index marks the gap between them.
- **In-progress marker:** while open, the file is named with a `.part.mp4` ending, then renamed to `.mp4` when closed. On startup, any `.part.mp4` left behind is still a playable fMP4: rename it, index it with `gap=true`, and queue it for copying.
- Same folder layout on every destination.

### 6.5 Hourly rollover and feed refresh (make-before-break)

At each hour boundary every camera:

1. At **HH:59:50**, opens a second RTSP session to the same camera.
2. At the first video keyframe received on the **new** session with wall time ≥ HH:00:00, opens the new hour's file and starts writing from that keyframe.
3. Closes the old file (writing all buffered packets up to the switch point), then tears down the old session.
4. If the new session fails to connect by HH:00:15: start the new hour's file on the **old** session at its next keyframe, log a warning, and retry the refresh at HH:05:00, then every 5 minutes.
5. If the camera refuses a second session (connection refused or RTSP 453/503): mark `singleSession=true` for that camera in memory. Thereafter do break-before-make at the hour (close, reconnect, open new file) and record the short gap in the index.

Small overlaps between the two sessions near the switch are acceptable. Gaps are not, except for single-session cameras.

### 6.6 Clock changes

- Use the monotonic clock for timeouts and the wall clock for file names.
- If wall time jumps by more than 5 s against the monotonic clock (NTP correction, manual change), close the current file at the next keyframe and start a new one named for the corrected time. Log an event.
- DST transitions are handled by the hour boundary plus the UTC offset in the name. Test both directions (Section 14).

---

## 7. Storage destinations

### 7.1 Staging

- Every camera always writes to the **staging** folder on a local disk. Staging is never a network path.
- The installer prompts for the staging path, and the System page can change it. Changing it applies at the next hour boundary for each camera. Existing files stay in the old location until moved or copied.

### 7.2 Copy flow

1. A worker closes a segment → guardian inserts it into `segments` and creates a `copies` row per destination (state `queued`).
2. The mover copies to `<dest>\<camera-id>\<YYYY-MM-DD>\<file>.part`, flushes, verifies size plus SHA-256, renames to the final name, then reports `verified`.
3. Once **every** destination of that camera reports `verified`, the staging copy is deleted.
4. A camera with **no** destinations keeps its files in staging, and staging retention applies (Section 8).

### 7.3 UNC shares

- Credentials: if the destination has a username, the mover calls `WNetAddConnection2` with those credentials for the share before copying. If not, it uses the service account's identity.
- Retry on failure: 30 s, 1, 2, 5, 10 min, then every 10 min forever. Recording is never affected.
- Destination status: `ok`, `retrying` (with last error and queue size), `disabled`.

### 7.4 Destination test

The UI's "Test" button: create the folder if needed, write a 1 MB file, read it back, compare, delete it, and report free space and total size. Show the exact Windows error on failure.

---

## 8. Retention

- Runs in the mover every 5 minutes and after each verified copy.
- **Size cap per destination (`capGB`):** if ButterCams' indexed files on that destination exceed the cap, delete the oldest segments (by `start`, across all cameras on that destination) until usage is under 95% of the cap.
- **Max age (`maxAgeDays`, optional):** delete segments older than this.
- **Disk-full safety:** if the destination volume's free space drops below 2%, act as if the cap were hit, even when under `capGB`.
- **Staging floor:** if staging free space drops below `max(minFreePercent, minFreeGB)` (whichever leaves more free space), delete the **oldest** staging files even if not yet copied, raise a **Critical** event naming every deleted file, and keep recording. Recording never stops for lack of space.
- **Protected segments:** the schema includes `protected` (always false in v1 and v2, set by users from v3). Retention must skip protected rows.
- Only delete files in the index. If a file is already gone, mark the row `deleted` and move on.
- Every delete is logged (path, size, reason).

---

## 9. Self-recovery

| Failure | Detection | Recovery |
| --- | --- | --- |
| Camera stream stalls | No video packet for 10 s | Close file, backoff, reconnect (6.1) |
| Camera offline / unreachable | Connect timeout or refused | Backoff forever; state `backoff` |
| Wrong credentials | RTSP 401 | State `error` with "Authentication failed"; retry every 60 s |
| Camera goroutine panics | `recover()` | Log stack, treat as stall for that camera only |
| Worker crashes | Process exit | Guardian restarts it within 2 s; cameras resume with `_b` files |
| Worker hangs | No heartbeat for 15 s | Guardian kills (Job Object terminate) and restarts it |
| Worker memory growth | `memBytes` > 512 MB | Restart the worker at the next hour boundary (after rollover) |
| Worker crash loop | 5 restarts in 10 min | Keep restarting with backoff up to 60 s, raise Critical, name the cameras |
| Mover crashes / hangs | Exit, or no heartbeat for 30 s | Restart; resume the queue from the database |
| Destination unreachable | Copy error | Queue and retry (7.3), Warning status |
| Staging nearly full | Free space below floor | Emergency retention (8), Critical status |
| Guardian crashes | Windows SCM | Service recovery: restart after 5 s on 1st, 2nd and subsequent failures; reset count after 1 day (set by installer) |
| Server reboot / power loss | — | Service starts automatically; `.part.mp4` files recovered (6.4) |
| Database corrupt | `PRAGMA quick_check` at startup fails | Rename the bad file, rebuild the index from disk (Section 10.4), raise Warning |
| Config unreadable | Parse error | Load newest good backup (5.3), Critical status |
| Clock jump | Wall vs monotonic drift > 5 s | New file at next keyframe (6.6) |
| Web UI crash | Panic in handler | Recovered per request; the UI is never allowed to affect recording |

Guardian-level `recover()` must wrap every goroutine it starts. A guardian panic that escapes must exit the process with a non-zero code so SCM recovery restarts it.

---

## 10. Health monitoring and logging

### 10.1 Self-check (every 60 s, in the guardian)

Checks:

1. Every enabled camera in state `recording` has a `lastPacketTs` within 30 s.
2. Every enabled camera that isn't `recording` has been in that state < 2 min (otherwise Warning), < 15 min (otherwise Critical).
3. Every worker and the mover have a heartbeat within their limits.
4. Staging free space is above the floor.
5. Every enabled destination's last copy attempt succeeded, or the queue is < 2 hours old.
6. Database writes succeed.

**Overall status:** `healthy`, `warning` or `critical`, with a list of reasons. Store transitions in the `events` table.

### 10.2 Watchdog of the recording itself

Every 60 s, for each camera in `recording`, confirm the current file's size on disk has grown since the last check. If not, treat it as a stall even if packets appear to arrive. This catches writes that silently fail.

### 10.3 Logs

- JSON lines in `<dataRoot>\logs\buttercams-YYYY-MM-DD.log`, kept 14 days (configurable 1–365), and capped at 2 GB total.
- Windows Event Log (source `ButterCams`): service start, stop, crash recovery, Critical status changes.
- Never log passwords or full RTSP URLs with credentials. Redact them as `rtsp://user:***@host…`.

### 10.4 Index rebuild

`buttercams.exe rebuild-index` and the System page button: scan staging and every destination for files matching the naming pattern, read the `udta` metadata and the MP4 duration, and rebuild the `segments` and `copies` tables. The result must match the original index for files on disk.

### 10.5 Database schema (SQLite, WAL, `foreign_keys=ON`)

```sql
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);  -- schema_version = 1

CREATE TABLE cameras (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  last_state TEXT,
  last_state_ts TEXT
);

CREATE TABLE segments (
  id INTEGER PRIMARY KEY,
  camera_id TEXT NOT NULL REFERENCES cameras(id),
  file_name TEXT NOT NULL,           -- name only; folder is derived
  start_ts TEXT NOT NULL,            -- RFC 3339 with offset
  end_ts TEXT,
  bytes INTEGER,
  sha256 TEXT,
  video_codec TEXT,
  audio_codec TEXT,
  gap_before INTEGER NOT NULL DEFAULT 0,
  close_reason TEXT,                 -- hour_rollover | stall | shutdown | crash_recovered | param_change | clock_jump
  protected INTEGER NOT NULL DEFAULT 0,
  UNIQUE (camera_id, file_name)
);
CREATE INDEX idx_segments_camera_start ON segments(camera_id, start_ts);

CREATE TABLE copies (
  segment_id INTEGER NOT NULL REFERENCES segments(id),
  destination_id TEXT NOT NULL,      -- 'staging' for the staging copy
  state TEXT NOT NULL,               -- queued | copying | verified | failed | deleted
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  updated_ts TEXT NOT NULL,
  PRIMARY KEY (segment_id, destination_id)
);

CREATE TABLE events (
  id INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,
  source TEXT NOT NULL,              -- guardian | worker:N | mover | camera:<id> | (future) ext:<name>
  type TEXT NOT NULL,                -- e.g. stream_stalled, worker_restarted, status_changed
  severity TEXT NOT NULL,            -- info | warning | critical
  camera_id TEXT,
  data TEXT                          -- JSON
);
CREATE INDEX idx_events_ts ON events(ts);
```

Events older than 90 days are pruned daily.

---

## 11. Web settings page and API

### 11.1 Network and security

- Listens on `0.0.0.0:8650` by default so **other PCs on the network can reach it**. The address and port can be changed.
- **HTTPS on by default.** At install, generate a self-signed certificate (`<dataRoot>\certs\`), valid 10 years, for the server's hostname, FQDN and IP addresses. Allow uploading a PFX or PEM certificate on the System page.
- Optional `allowedSubnets` restricts which client IPs can connect.
- **Login required.** The installer creates the first admin account (Section 13). Passwords hashed with argon2id. Sessions use a secure, HttpOnly, SameSite=Strict cookie, expiring after 12 h idle.
- Two roles: **Admin** (everything) and **Viewer** (read status, list and download recordings).
- Lock an account for 15 minutes after 5 failed logins.
- CSRF protection on all state-changing requests.
- The web server runs in the guardian process, but every handler has a 30 s timeout and panic recovery, and it never calls into workers synchronously.

### 11.2 Pages

1. **Dashboard:** overall status with reasons; a table of cameras (name, state, bitrate, resolution, codec, audio, last packet age, current file size); destinations (status, used / cap, queue length); staging free space. Auto-refreshes every 5 s.
2. **Cameras:** list; add, edit, enable or disable, delete (with "keep recordings" confirmation).
   - Fields: name, host, port, path (or a pasted full URL), username, password, transport, record audio, destinations.
   - **Test connection** button: connect once, report video codec, resolution, fps, keyframe interval, audio codec, and whether a second session is allowed. Warn if the keyframe interval is over 2 s: "Set the camera's I-frame interval to 1–2 seconds for clean hourly rollovers."
   - **Bulk import** from CSV: `name,host,port,path,username,password,destinations`.
3. **Destinations:** list; add or edit (name, type, path, username, password, cap GB, max age days); **Test** button (7.4); used space and queue.
4. **Recordings:** choose camera and date; list hour files with start, end, size, gap flags and where each copy lives; download links (Viewer and Admin).
5. **Events:** filterable log of the `events` table.
6. **System:** staging path and floor; port, address, allowed subnets; certificate; users; log retention; export config (passwords removed) and import; rebuild index; version and uptime.

All forms validate on the client and the server. Use plain, specific error messages.

### 11.3 REST API (versioned under `/api/v1`, JSON)

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/health` | Overall status and reasons (no auth required if called from localhost; otherwise auth) |
| GET | `/api/v1/cameras` | Cameras with live stats |
| POST | `/api/v1/cameras` | Create |
| PUT | `/api/v1/cameras/{id}` | Update |
| DELETE | `/api/v1/cameras/{id}` | Delete |
| POST | `/api/v1/cameras/test` | Test a camera config without saving |
| GET/POST/PUT/DELETE | `/api/v1/destinations[/{id}]` | Destinations CRUD |
| POST | `/api/v1/destinations/test` | Test a destination |
| GET | `/api/v1/segments?camera=&date=` | List segments |
| GET | `/api/v1/segments/{id}/download` | Stream the file from the nearest available copy |
| GET | `/api/v1/events?since=&severity=&camera=` | Events |
| GET | `/api/v1/system` | Version, uptime, disk, config summary |
| PUT | `/api/v1/system` | Update system settings |
| POST | `/api/v1/system/rebuild-index` | Start rebuild |
| GET | `/api/v1/stream` | Server-Sent Events: stats and status changes |
| POST | `/api/v1/auth/login`, `/logout` | Session |

The API is the contract later versions build on. Add endpoints freely later; never change existing ones without a new version prefix.

---

## 12. Performance targets (60 cameras)

| Measure | Target |
| --- | --- |
| CPU, all ButterCams processes | Under 5% average on a modern 8-core server CPU (excluding audio transcoding, which must stay under 1% per camera) |
| Memory, all processes | Under 1.5 GB total; flat over 30 days |
| Write throughput | Keeps up with total camera bitrate (e.g. 60 × 4 Mbps ≈ 30 MB/s) |
| Gap at hourly rollover | 0 s on cameras that allow two sessions |
| Recovery after camera returns | Recording within 15 s |
| Video lost on crash or power loss | Under 3 s per affected camera |
| UI response | Pages under 500 ms on the LAN |

Guidance: reuse packet buffers (`sync.Pool`), avoid per-packet allocations, use buffered file writes, and keep one goroutine per camera plus the I/O path.

---

## 13. Installer and Windows integration

The MSI must:

1. Ask for the **data root** (default `C:\ProgramData\ButterCams`) and the **staging folder** (default `<dataRoot>\staging`). Warn if the staging drive has under 100 GB free. Warn if it's the system drive.
2. Optionally ask for a first destination (local path or UNC share plus credentials, with a Test button). This can be skipped and done later in the web UI.
3. Ask for the web port (default 8650) and create the first **admin username and password**.
4. Ask which account runs the service: **LocalSystem** (default) or a specified domain/local account (recommended when writing to network shares without per-destination credentials). Grant "Log on as a service" if needed.
5. Install files, register the service, set **recovery actions** (restart after 5 s on every failure, reset after 1 day) and **Automatic (Delayed Start)**.
6. Add a Windows Firewall inbound rule for the web port, TCP, on **Domain and Private** profiles only.
7. Generate the self-signed certificate.
8. Add a Start-menu shortcut "ButterCams Settings" that opens `https://localhost:<port>`.
9. Start the service and open the settings page.

Upgrades keep `config.json`, the database, certificates and recordings. Uninstall asks whether to keep or remove the data root. **Never delete recordings on uninstall unless the user explicitly chooses it.**

---

## 14. Testing and acceptance criteria

### 14.1 Test tooling to build

- **Camera simulator:** a test harness (use MediaMTX or a small Go RTSP server) that serves N looping streams from sample files, with switches for: H.264, H.265, AAC, G.711, no audio, 1 s / 4 s keyframe interval, single-session only, random stalls, auth failure.
- **Chaos script:** kills workers, mover and the guardian at random; blocks a share via firewall; fills the staging disk; changes the system clock.

### 14.2 Automated tests

- Unit tests: config validation and migration, file naming (including DST both directions and the `_b` suffix), retention selection, backoff timing, state machine transitions, redaction of secrets in logs.
- Integration tests with the simulator: hourly rollover with no gap; single-session fallback; stall recovery; crash recovery of `.part.mp4`; copy plus verify plus staging cleanup; retention at cap; index rebuild equality.
- Run in CI on a Windows runner.

### 14.3 v1 acceptance criteria

- [ ] 60 simulated cameras for 72 h within the CPU and memory targets.
- [ ] Garrett's real camera models record correctly, including audio and the hourly rollover.
- [ ] Every chaos scenario recovers automatically, with loss within the targets.
- [ ] DST fall-back and spring-forward produce correct, unique files.
- [ ] Share offline for 6 h: recording uninterrupted; backlog copies when it returns.
- [ ] Staging fills: oldest files removed, recording continues, Critical raised.
- [ ] Deleting the database and rebuilding gives an identical segment list.
- [ ] Sample files from every camera play in VLC and Windows Media Player, including a crash-cut file.
- [ ] Settings page works from another PC over HTTPS with login.
- [ ] 30-day soak on the target Windows Server with no missing hours and flat memory.

---

## 15. v1 hooks for later versions

v1 includes these so v2–v4 can be added **without changing the recorder**:

- `subStreamPath`, `recordingMode`, `extensions` config fields (saved and loaded, unused).
- The `events` table and event JSON format; `source` accepts `ext:<name>`.
- The `protected` column on segments.
- The guardian's process supervision is generic: workers and the mover are both "supervised processes" defined by a struct (executable, args, heartbeat limit, restart policy), so extension processes can be added later with the same code.
- The worker exposes a packet fan-out interface per camera (a function that registers a reader receiving packets on a **bounded channel that drops when full**). In v1 only the file writer uses it; v2 adds the loopback restream (16.3). **The recorder must never block on a reader.**
- API versioned under `/api/v1`.

---

## 16. Version 2 — Live View and Timeline Playback

v2 lets people watch: live video from every camera, and recorded video on a timeline, in the browser, from the same page and login as the settings. It is the next phase of development after v1. The build order, the code changes and the tests are in `docs/V2_PLAN.md`.

Viewers only read. They never write to the core, and they can never slow it.

### 16.1 Principles

1. **The server never decodes video.** Rule 2 holds in v2. The server repackages the camera's packets (live) or sends byte ranges of the recorded files (playback). Decoding happens in the browser.
2. **One port, one login.** Live video and playback use the existing HTTPS port, session cookie and roles. There are no extra ports, no second web server and no browser plug-in.
3. **Viewing is a separate process.** Everything that serves video to browsers runs in one supervised process, the **media process**. If it crashes, hangs or is overloaded, recording, copying and the settings page carry on.
4. **Nothing runs when nobody watches.** Stream sources start on the first viewer and stop 30 s after the last one leaves. With no viewers, v2 costs the server nothing.
5. **Nothing extra to install.** No go2rtc, no ffmpeg, no CGO. The media process is `buttercams.exe media`.

### 16.2 Media process

A third process role, supervised by the guardian with the same code as workers and the mover (Section 15):

```
buttercams.exe media        # live view and playback server (spawned by the guardian)
```

```
 Cameras ─ main ─► Recorder workers ─► files ─────────────────────────┐
    │                  │ loopback restream (on demand, main only)     │ read-only
    │                  ▼                                              ▼
    └─── sub ───►  Media process: stream hub ─ live muxer ─ playback file server
                       ▲ loopback HTTP + token
 Browser ─HTTPS─►  Guardian: login, permissions, timeline from the index, reverse proxy
```

- Listens on `127.0.0.1` only, on a port it chooses, and reports the port to the guardian. Every request must carry a token the guardian generated for that run of the process.
- The guardian checks the session, the role and the camera permission, then reverse-proxies the request. The media process trusts only the guardian.
- It never opens the database and never writes to staging, destinations or the data root. For playback, the guardian resolves a segment to its file paths (nearest copy first, as for downloads) and passes them with the proxied request.
- Heartbeat every 5 s; killed and restarted after 15 s without one. Restarted when it uses more than 1 GB of memory. A restart at any moment is safe: viewers reconnect.
- Runs at **below-normal process priority**, so when the CPU is busy the recorders win.
- Receives the camera list (with decrypted-on-use credentials for sub streams) and each worker's restream address from the guardian over stdin, like a worker receives its cameras.

### 16.3 Stream sources

| Stream | Where the media process gets it | Notes |
| --- | --- | --- |
| **Sub** | Its own RTSP connection to the camera's `subStreamPath` | The recorder workers are not involved at all. Same connect timeout, stall detection and backoff as Section 6.1. Never recorded |
| **Main** | The worker's **loopback restream** | Never a second connection to the camera: many cameras allow few sessions, and the hourly rollover needs one |

**Loopback restream (in each worker).** A loopback-only RTSP server, `rtsp://127.0.0.1:<port>/<camera-id>`, fed by the packet fan-out of Section 15.

- It registers a fan-out reader only while a client is connected, and unregisters when the client leaves.
- One client per RTSP session of the camera (the media process, which fans out to all viewers itself). A camera has two sessions only during the hourly rollover, so at most two clients. Further connections are refused.
- Accepts connections from 127.0.0.1 only and requires the token the guardian gave the worker.
- The reader's channel is bounded and drops when full. Dropped packets are counted and reported in `camera_stats`. **The recorder never waits.**
- **No freeze at the hourly rollover.** The restream serves each RTSP session of a camera as its own stream; `rtsp://…/<camera-id>` always describes the newest. When a camera opens its second session (6.5), the worker reports it (`restream_session`), and the media process connects to it while still playing the old one. It moves viewers to the new session at that session's first keyframe, then drops the old one: make-before-break, like the recorder, and decided in the media process so the worker needs no switching logic. If the notice does not arrive, or the camera is single-session or reconnects after a failure, the old stream simply ends and the media process connects again; live view then resumes at the next keyframe.
- Every goroutine of the restream recovers from panics; a failure in it is logged and disables the restream for that camera until the next session. It can never end a recording.
- `live.mainStream: false` (16.8) turns the restream off entirely.

**Stream hub (in the media process).** One source per camera and stream, shared by all its viewers. The hub keeps the packets since the last keyframe, so a new viewer gets a picture at once instead of waiting for the next keyframe.

### 16.4 Live view

| Requirement | Detail |
| --- | --- |
| Transport | Fragmented MP4 over a WebSocket, played with Media Source Extensions (MSE; `ManagedMediaSource` on iPhone) |
| Browsers | Chrome on Windows and Android, Safari on iPhone (iOS 17.1 or later) and Mac. Edge and Firefox are expected to work but are not acceptance targets |
| Phones | The Live page and the timeline work by touch on a phone-size screen. The grid shows at most 4 tiles per page there (swipe for more); video plays inline and muted without a tap |
| Muxing | One fragment per video frame, so the server adds no delay. Same muxer code as the recorder |
| Latency | Under 1 s on the LAN. The player keeps itself at the live edge: it speeds up slightly when it is more than 1 s behind and jumps when more than 3 s behind, and it trims its buffer, so delay never grows |
| Grid | Layouts 1, 4, 9 and 16 and saved views (16.6), using sub streams. Sub streams are expected to be H.264. A camera without a sub stream uses its main stream in the grid, and the camera form recommends setting one |
| Single view | Click a tile for the main stream, full screen available |
| H.265 | Played where the browser can (`MediaSource.isTypeSupported`). Otherwise the tile falls back to the sub stream if that is H.264. Otherwise it says which browsers can play it. The server never transcodes video |
| Audio | In single view only, muted by default. AAC and Opus are passed through; G.711/G.726 are converted to AAC with the recorder's converter (Section 6.3), only while someone is watching |
| Slow viewers | Each viewer has a bounded queue. When it overflows, the queue is emptied and the viewer resumes at the next keyframe. A slow viewer affects nobody else |
| Hidden tiles | The page closes the stream of every tile that is not visible (other page of the grid, background tab) and reopens it when it is shown |
| Not available | When the source is not available (camera offline, no sub stream) the tile shows the camera's recording state and reason, as the v1 Live page does, and retries by itself |
| Limits | 4 streams per camera in flight per user session; 200 media connections in total. Beyond that the server answers 503 and the tile says so |

WebSocket protocol (`GET /api/v1/live/{camera}/{main|sub}?audio=0|1`): the server sends a text message `{"type":"init","mime":"video/mp4; codecs=\"…\"","width":…,"height":…}` followed by binary messages: the initialization segment, then fragments. A new `init` message follows whenever the stream's parameters change. `{"type":"state","state":"waiting","message":"…"}` is sent while there is no video. The client sends nothing. The handler checks the `Origin` header.

### 16.5 Timeline playback

The **Recordings** page becomes the timeline. The v1 file list stays as a second tab ("Files").

**Timeline.**

- One camera (or up to four, see below) and a date. Recorded spans, gaps and recorder events (stalls, reconnects, clock changes) are drawn in different colours, each with a text label on hover. Zoom from 24 hours down to 1 minute.
- The hour currently being recorded is on the timeline and can be played up to about 5 s behind live.
- Days with a clock change show 23 or 25 hours correctly.
- Motion events and AI labels are added to the same timeline by v3 and v4.

**Playback.**

- Click or drag anywhere to play from that moment. Playback crosses hour files with no visible seam.
- The browser builds the continuous stream itself. For each file it asks for a small **index** (the initialization segment's byte range, and the time and byte range of every keyframe-aligned chunk), then fetches chunks with ordinary HTTP range requests and appends them through MSE. The server holds no playback state.
- Files that follow each other without a gap are joined end to start on the player's timeline by their exact media durations; the wall-clock time shown is mapped per file. A real gap is skipped, and the player says what was skipped ("No recording 09:12:04 – 09:14:31").
- When the next file has a different codec, resolution or set of tracks, the player starts a new media source at the boundary. A short blink is acceptable there.
- Speeds 0.5×, 1×, 2×, 4×, 8× and 16×. Up to 4× (and above when the browser keeps up) every frame is played. Otherwise the player switches to **keyframe-only** mode: the server returns fragments holding just the first frame of each chunk, with its duration stretched to the chunk's. The same mode serves fast scrubbing while dragging.
- Frame step forward and back while paused.
- Audio plays at 0.5× to 2×.
- **Synchronized playback** of up to 4 cameras: one shared wall clock, each player kept within 150 ms of it. A camera with no recording at that moment says so and joins again at its next span.
- **Thumbnails on hover:** the server returns a tiny, complete MP4 holding one keyframe; the browser decodes it and caches the picture. The server decodes nothing.
- **Download** the hour file under the cursor (the v1 endpoint). Trimmed exports of a range and protection from retention come with v3 (17.4); the timeline's range selection is built so that export plugs into it.
- H.265 recordings play where the browser can play H.265; otherwise the player says so and offers the download. So that Safari can play them, H.265 files are written with the `hvc1` sample entry from v2 on (all parameter sets in the file header, which the recorder already guarantees by starting a new file when they change).

**Server side.**

- Closed files: the index is read from the file's `mfra` box (one small read at the end of the file).
- The file being written has no `mfra` yet: its index is built by walking the fragment headers, kept in memory and extended on each request.
- Indexes are cached in memory (least recently used, bounded).
- Files are opened read-only with full sharing **including delete** (16.9).

### 16.6 Access

- **Roles:** Viewer and Admin can use live view and the timeline.
- **Per-user camera permissions:** a Viewer account can be limited to a list of cameras (default: all). The limit applies everywhere a camera appears: camera list, live view, timeline, segments, downloads, events and the event stream. Admins always see every camera.
- **Saved views** move from the browser to the server. Every user has **their own** views, which only they see and edit. Admins can also keep **shared** views, which every user sees and only Admins edit; an Admin can share one of their own views with one click. A user sees only the cameras of a shared view that they are allowed to see. Views saved in a browser by v1 are offered for import into the user's own views once.
- Stream starts and playback starts are not audited (they are too frequent). Downloads are audited from v3 (17.4).

### 16.7 Remote access

- No built-in relay and no internet-facing ports by default.
- Because live view and playback use only the HTTPS port, anything that carries that port carries them. `docs/OPERATIONS.md` documents access over Tailscale or WireGuard.

### 16.8 Configuration, data and API changes

`config.json` → `schemaVersion: 2`, with a migration from v1 that adds:

```json
"live": {
  "enabled": true,
  "mainStream": true,
  "views": [
    {"id": "view-entrances", "name": "Entrances", "layout": 4, "cameraIds": ["front-door", "loading-dock"]}
  ]
}
```

- `live.enabled: false` stops the media process and hides live view and playback. `live.mainStream: false` keeps the workers' restream off (grid and single view then use sub streams only).
- `subStreamPath` (reserved in v1) is now used. The camera form gets a "Sub stream path" field, and **Test connection** also tests the sub stream and reports its codec and resolution. It warns when the sub stream is H.265 ("Set the sub stream to H.264 so every browser can show the grid") or larger than 1280×720.
- `live.views` holds the **shared** views. `users.json`: each user gains `cameras` (`null` = all cameras, or a list of camera ids) and `views` (that user's own views, same shape, at most 50).
- The database is unchanged.

New messages (Section 4.4): guardian → worker `restream` (enable or disable, token); worker → guardian `restream_ready` (port) and `restream_session` (camera, new session); `camera_stats` gains `restreamDropped`. Guardian ↔ media: `assign_media` (cameras, restream addresses, token), `source_changed` (the forwarded `restream_session`), `media_ready` (port), `heartbeat`, `media_stats` (viewers and sources per camera, queue overflows), `event`.

New API endpoints under `/api/v1` (existing ones do not change):

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/live/{camera}/{main\|sub}` | WebSocket live stream (16.4) |
| GET | `/live/views` | `{"shared": […], "mine": […]}` |
| PUT | `/live/views/mine` | Replace the signed-in user's own views |
| PUT | `/live/views/shared` | Replace the shared views (Admin) |
| GET | `/timeline?camera=&from=&to=` | Recorded spans (segment id, start, end, gap flag, codecs, open flag) and recorder events in the range, from the index |
| GET | `/segments/{id}/play/index` | Playback index of one file: MIME type with codecs, initialization range, chunk list `[startMs, durationMs, offset, size]`, `open` flag |
| GET | `/segments/{id}/play/data` | The file's bytes, range requests only |
| GET | `/segments/{id}/play/keyframes?from=&to=` | Keyframe-only fragments for a time range |
| GET | `/segments/{id}/play/thumb?t=` | One keyframe as a complete small MP4 |

Additions to existing objects: `subStreamPath` in CameraInput; `sub` results in CameraTestResult; `cameras` in users; `media` (state, viewers, sources) in `GET /system` and on the Dashboard. The `/live/…` and `/play/…` routes are long-lived like downloads and are exempt from the 30 s handler timeout; the guardian gives the media process 2 s to accept a proxied request and otherwise answers `503 media_unavailable`.

### 16.9 Protecting the recorder

These apply Rule 1 to v2. Each has a test in `docs/V2_PLAN.md`.

1. **Process isolation.** All viewer work is in the media process (16.2), at below-normal priority, with a memory limit.
2. **The restream is the only v2 code inside a worker.** It is idle unless watched, serves one client per camera session, drops instead of waiting, recovers from its own panics and can be switched off (16.3).
3. **Fan-out packets are read-only.** A reader that needs to change a packet copies it first.
4. **Shared file access.** Every read of a recording by the guardian or the media process (playback, thumbnails, downloads, including the v1 download) opens the file with read, write **and delete** sharing. The worker's rename of `.part.mp4` at close, the mover's deletion of the staging copy and retention's deletes must never fail because someone is watching. The media process closes file handles that have been idle for 30 s, so a deleted file's space is released promptly.
5. **Read budget.** Playback reads from the volume that holds staging share one budget (default 40 MB/s in total). When it is used up, viewers load more slowly; recorders keep writing at full speed.
6. **The guardian only forwards.** It copies bytes between the browser and the media process without buffering, limits the number of media connections, and still never calls into a worker synchronously.
7. **Sub-stream connections belong to the media process.** A camera that misbehaves on its sub stream cannot disturb its recording.

### 16.10 v2 performance targets

| Measure | Target |
| --- | --- |
| Live latency on the LAN | Under 1 s, and not growing over 8 hours |
| Time to first picture, live | Under 2 s for a sub stream that is already open, under 5 s when it must be connected |
| Time to first picture after a seek | Under 1 s from a local copy, under 2 s from a network share |
| 16 live tiles for 8 hours | No growing delay and flat browser memory in a mid-range laptop browser |
| 4 live tiles on a phone for 1 hour | No growing delay, on a mid-range Android phone and an iPhone |
| Main-stream live view at the hourly rollover | No freeze longer than 0.5 s on cameras that allow two sessions |
| Media process, 60 sub streams and 20 viewers | Under 5% of an 8-core CPU, under 500 MB |
| Recorder CPU and memory | No measurable change from v1 with 20 viewers watching and scrubbing |

### 16.11 v2 acceptance criteria

- [ ] Seamless playback across 24 consecutive hour files.
- [ ] 16 live tiles run for 8 hours with latency under 1 s at the end.
- [ ] 20 concurrent viewers, live and scrubbing, cause no recording loss and no change in recorder CPU or memory.
- [ ] Killing the media process repeatedly during live viewing does not affect recording, and live view returns within 10 s.
- [ ] A media process pinned at 100% CPU does not slow any recorder.
- [ ] On Windows, the hourly rollover, staging cleanup and retention all succeed while the affected files are being played.
- [ ] The hour being recorded plays to within 5 s of live.
- [ ] A viewer limited to some cameras cannot reach any other camera through any endpoint.
- [ ] Timeline and playback are correct on both clock-change days.
- [ ] Live view and playback work in Chrome on Windows, Chrome on Android and Safari on an iPhone (iOS 17.1 or later), on the LAN and through Tailscale.
- [ ] A main-stream live view left open across the top of the hour shows no visible freeze on a camera that allows two sessions.
- [ ] One user's own views are invisible to other users; a shared view shows each user only the cameras they may see.
- [ ] Garrett's camera models play live (main and sub) and from the timeline, including audio.

---

## 17. Version 3 — Alerts, Clip Export and Motion Detection

v3 adds three things: alerts when something breaks, trimmed exports and protection of recordings, and motion detection. All three run beside the recorder. If any v3 component fails, recording continues exactly as in v1.

### 17.1 Extension host

Every v3+ feature that processes video or sends messages runs as a **supervised extension process**, managed by the guardian with the same supervision code as workers (Section 15).

- Each extension has a manifest, `extensions\<name>\manifest.json`:

```json
{
  "name": "motion",
  "version": "1.0.0",
  "executable": "buttercams-motion.exe",
  "args": [],
  "heartbeatTimeoutSec": 30,
  "readsStreams": ["sub"],
  "emitsEvents": ["motion.start", "motion.end"],
  "settingsSchema": "settings.schema.json",
  "maxMemoryMB": 1024
}
```

- The guardian starts every enabled extension, restarts it with backoff on exit or missed heartbeat, and shows its status on the Dashboard.
- Extensions talk to the guardian over a loopback HTTP API with a per-process token passed in an environment variable: register, heartbeat, post events, read settings, list cameras.
- Each extension's settings are edited on a page generated from its `settingsSchema` (JSON Schema), so new extensions don't need hand-built settings pages.
- Extensions may never write to staging, destinations or the database directly.

### 17.2 Streams for extensions

- Extensions read streams from the v2 stream hub (16.3), never from cameras and never from the workers. The media process gains a loopback-only RTSP output, `rtsp://127.0.0.1:<port>/<camera-id>/main` and `/sub`, which accepts connections only from 127.0.0.1 and requires the extension's token.
- A sub stream that an enabled extension reads is kept connected permanently instead of on demand.
- Each reader has a bounded, drop-when-full queue. Dropped packets are counted and shown per reader.
- The recorder workers do not change.

### 17.3 Alerts

A supervised process, `buttercams-alerts.exe`, subscribes to the guardian's event stream.

| Requirement | Detail |
| --- | --- |
| Channels | Email (SMTP with STARTTLS or TLS), ntfy (server URL, topic, optional token), webhook (JSON POST with an optional HMAC signature header) |
| Built-in triggers | Camera not recording for over N minutes (default 5); worker restarted over 3 times in an hour; destination failing for over N minutes (default 15); staging floor reached; service restarted; overall status changed to Critical; motion or object event (opt-in per camera) |
| Resolved notices | Send a "resolved" message when a condition clears |
| Flood control | One alert per condition until resolved; more than 5 alerts within 10 minutes become one digest |
| Quiet hours | Per channel; Critical alerts always send |
| Daily summary | Optional: hours recorded per camera, gaps, storage used per destination |
| Test | "Send test" button per channel |
| Storage | Channel settings in `config.json` under `alerts`, secrets encrypted with DPAPI |

Message format: subject or title `ButterCams [<server>] <severity>: <short reason>`, body with camera, time, details and a link to the camera's timeline at that moment.

### 17.4 Clip export and protection

Browsing and in-browser playback exist since v2 (16.5). v3 adds, on the timeline:

- **Trim and export:** select a start and end time spanning one or more hour files. Produce one MP4 by cutting at the nearest keyframes and concatenating, with no re-encoding. Exports run in a supervised process, are written to `<dataRoot>\exports\`, expire after 7 days by default, and are listed with a download link.
- **Protect:** mark a segment (or a time range) as protected, with an optional note. Retention skips protected segments. Protected bytes show separately against each destination's cap, with a warning when they exceed 50% of it. Protected spans are marked on the timeline.
- **Audit log:** every export, download, protect and unprotect is recorded in the `events` table with the user.

New API endpoints under `/api/v1`: `POST /exports`, `GET /exports`, `GET /exports/{id}/download`, `POST /segments/{id}/protect`, `DELETE /segments/{id}/protect`.

### 17.5 Motion detection

A supervised Go extension, `buttercams-motion.exe`, reading sub streams from the stream hub.

| Requirement | Detail |
| --- | --- |
| Input | The camera's sub stream. Cameras without `subStreamPath` can't use motion; the UI says so |
| Decode | Hardware decode where available (Intel Quick Sync, NVIDIA NVDEC, AMD AMF via D3D11VA), CPU fallback. Decoding happens only in this process |
| Rate | Analyze 5 frames per second by default (2–15 configurable) |
| Method | Downscale to about 320 px wide grayscale; running-average background model; per-pixel difference threshold; morphological cleanup; connected regions |
| Zones | Include and exclude polygons per camera, drawn over a snapshot in the UI |
| Sensitivity | Minimum changed area (% of zone), minimum duration (ms), cooldown (s), per camera |
| Noise rejection | Ignore frames where more than 60% of pixels change at once (IR switch, headlights, exposure jumps), and reset the background model |
| Output | `motion.start` and `motion.end` events with camera, time, peak area, bounding box and a JPEG snapshot saved under `<dataRoot>\snapshots\` |
| Snapshot retention | 30 days by default, capped at 10 GB |

### 17.6 Recording modes

`recordingMode` gains two values:

| Mode | Behaviour |
| --- | --- |
| `continuous` (default) | As v1. Motion events are markers only |
| `continuous_marked` | As v1. Hours containing motion are flagged; retention deletes unflagged hours first when a destination is full |
| `motion_only` | The recorder keeps a rolling in-memory buffer of the main stream (default 10 s, keyframe-aligned). On `motion.start`, it writes the buffer and keeps recording until a post-roll (default 15 s) after `motion.end`. Files still follow the hour naming scheme; an hour may hold several `_b`, `_c` files |

Safety rule: if the motion extension is down, not heartbeating, or the camera's sub stream is unavailable, every `motion_only` camera switches to continuous recording until motion is healthy again, and an event is logged.

### 17.7 Motion on the timeline, and event search

- Motion events are drawn on the v2 timeline in their own colour.
- **Event search:** filter by camera, time range and event type. Results are shown as snapshot thumbnails; clicking one opens the timeline at that moment.

### 17.8 Data and API changes

- `config.json` → `schemaVersion: 3` with a migration from v2: adds `alerts`, motion settings under each camera's `extensions.motion`, and the new recording modes.
- `segments` gains `motion_flag INTEGER NOT NULL DEFAULT 0`. New table `exports (id, user, camera_id, start_ts, end_ts, path, bytes, created_ts, expires_ts)`.
- New event types: `motion.start`, `motion.end`, `alert.sent`, `alert.failed`, `export.created`, `segment.protected`, `segment.unprotected`.
- `GET /timeline` also returns motion events and protected spans.

### 17.9 v3 performance targets

- Motion analysis of 60 sub streams at 5 fps: under 25% CPU on the target server with hardware decode.
- Motion event within 1 s of motion starting.
- Recorder CPU and memory unchanged from v1 with every v3 component running.

### 17.10 v3 acceptance criteria

- [ ] Killing the alerts, export or motion process repeatedly causes no recording loss; each is restarted automatically.
- [ ] A motion process pinned at 100% CPU does not slow any recorder.
- [ ] With motion down, `motion_only` cameras switch to continuous within 10 s.
- [ ] Every alert trigger fires and resolves in the chaos suite; the mail server or webhook target offline for 24 h has no effect on recording.
- [ ] An export spanning three hour files plays end to end in VLC with no visible seam.
- [ ] Protected segments survive retention at full cap.
- [ ] Motion-only clips include the full pre-roll in every test case.
- [ ] False-positive rate measured on a week of Garrett's footage meets a target agreed with him.

---

## 18. Version 4 — AI Object Detection

v4 confirms motion with object detection (person, vehicle, animal, package) in a Python extension, turning motion markers into labelled events. It uses the v3 extension host and event channel and the v2 stream hub, with no change to the recorder.

### 18.1 AI extension

| Requirement | Detail |
| --- | --- |
| Packaging | Python 3.12+, frozen with an embedded runtime so nothing must be installed on the server; runs as a supervised extension `buttercams-ai` |
| Runtime | ONNX Runtime, choosing CUDA, then DirectML, then CPU at startup and reporting which is active |
| Models | YOLO-family ONNX object detection models. Ship one default general model; users can add custom `.onnx` models with a `labels.txt` |
| Trigger | By default, on each `motion.start`: analyze 3 frames spread over the first 2 s, then 1 frame per second until `motion.end` (max 30 per event). Optional continuous mode per camera at a set rate |
| Frames | Sub-stream frames from the stream hub; optionally fetch a main-stream keyframe for small or distant objects |
| Output | `object.detected` events: camera, time, label, confidence, bounding box, snapshot with boxes drawn, and the `motion.start` event id they belong to |
| Filters per camera | Labels to keep, minimum confidence (default 0.5), include/exclude zones, minimum box size |
| Stationary objects | Track objects across frames by box overlap; ignore objects that haven't moved over 3 consecutive detections (parked cars) |
| Queue | Bounded per camera and overall. When full, drop the oldest requests and count them. Never block upstream |
| GPU memory | Configurable limit; on out-of-memory, fall back to CPU and raise a Warning |
| Remote mode | Optional: send frames to a ButterCams AI extension on another machine over HTTPS with a shared token |

### 18.2 Model management

- **Models** page: list installed models, upload a new one, set the default and per-camera models, and test a model on a chosen snapshot with boxes shown.
- Switching models takes effect without restarting the service.

### 18.3 Using AI results

- Alerts (17.3) can filter by label, for example "person on Driveway only".
- `continuous_marked` and `motion_only` modes can require an AI-confirmed label instead of raw motion (setting per camera).
- The timeline (16.5) shows AI labels in their own colour. Event search (17.7) gains filters for label and minimum confidence, and shows the labelled snapshots.

### 18.4 Data changes

- `config.json` → `schemaVersion: 4`: adds `extensions.ai` per camera and a top-level `ai` section (runtime, models, remote mode).
- New table `detections (id, event_id, camera_id, ts, label, confidence, x, y, w, h, snapshot_path, model)` with an index on `(camera_id, ts)` and `(label, ts)`.

### 18.5 v4 performance targets

- Under 300 ms per frame on an RTX 4060 Ti-class GPU with the default model.
- No queue drops at normal activity across 60 cameras on that GPU.
- Recorder CPU and memory unchanged from v1.

### 18.6 v4 acceptance criteria

- [ ] Removing the GPU or its driver falls back to CPU (or disables AI with a Warning) with no recording impact.
- [ ] A model that crashes or loads slowly is restarted without affecting motion events or recording.
- [ ] Person-detection precision and recall measured on Garrett's day and night footage meet targets agreed with him.
- [ ] Model changes apply without a service restart.

---

## 19. Repository layout

```
buttercams/
  cmd/buttercams/main.go          # sub-command dispatch
  internal/guardian/              # service, supervision, health, config owner
  internal/worker/                # camera state machines, RTSP, fMP4 writer
  internal/mover/                 # copy, verify, retention
  internal/config/                # schema, validation, migrations, DPAPI
  internal/index/                 # SQLite access, rebuild
  internal/ipc/                   # JSON-lines messages
  internal/api/                   # HTTP server, auth, handlers
  internal/winsvc/                # service, job objects, event log, WNet
  internal/audio/                 # G.711/G.726 → AAC
  internal/stream/                # v2: RTP → samples, shared by the recorder and the media process
  internal/restream/              # v2: loopback RTSP restream inside a worker
  internal/media/                 # v2: media process (stream hub, live muxer, playback file server)
  internal/exthost/               # v3: extension manifests, supervision, loopback API
  extensions/alerts/              # v3 (Go)
  extensions/export/              # v3 (Go)
  extensions/motion/              # v3 (Go)
  extensions/ai/                  # v4 (Python)
  web/                            # React + Vite UI (built output embedded)
  web/src/player/                 # v2: live and timeline players (MSE)
  installer/                      # installer project
  test/simulator/                 # camera simulator
  test/chaos/                     # chaos scripts
  docs/
    DECISIONS.md                  # choices made during the build
    IDEAS.md                      # out-of-scope ideas
    OPERATIONS.md                 # admin guide (install, add camera, recover)
    V2_PLAN.md                    # how v2 is built: order, code changes, tests
  AGENTS.md                       # points agents at this spec
```

---

## 20. Glossary

- **RTSP:** the protocol IP cameras use to stream video.
- **Main stream / sub stream:** a camera's full-quality and low-resolution streams.
- **Keyframe (I-frame):** a complete frame that playback can start from. Files and fragments start on keyframes.
- **fMP4 (fragmented MP4):** MP4 written in small self-contained pieces, so a file cut off mid-write still plays.
- **Staging:** the local folder every camera writes to first.
- **Destination:** where finished hour files are copied (local, attached or network share).
- **Make-before-break:** opening the new stream before closing the old one, so there's no gap.
- **MSE (Media Source Extensions):** the browser feature that lets a page feed fMP4 pieces to a video element. It is how live view and playback work without a plug-in.
- **Media process:** the supervised process that serves live video and playback to browsers (Section 16.2).
- **Restream:** a worker's loopback-only copy of a camera's main stream, read by the media process.
- **Stream hub:** the part of the media process that holds one connection per stream and shares it among viewers.
- **Live edge:** the newest video a live player has received. The player stays close to it.
