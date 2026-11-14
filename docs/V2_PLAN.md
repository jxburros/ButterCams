# Building v2: Live View and Timeline Playback

`BUTTERCAMS_SPEC.md` section 16 says **what** v2 must do. This document says
**how** to build it: the order, the code that changes, the tests, and the
risks. Nothing here is built yet.

## 1. The shape of the solution

```
 Cameras ─ main ─► Recorder workers ─► hour files (staging, destinations)
    │                  │ loopback restream                    │
    │                  │ (on demand, one client)              │ read-only, shared
    │                  ▼                                      ▼
    └─── sub ───►  buttercams.exe media
                     stream hub ─► live muxer ─► WebSocket
                     playback file server (index, ranges, keyframes)
                       ▲ 127.0.0.1 + token
 Browser ─HTTPS─►  Guardian: session, role, camera permission,
                     /timeline from the index, reverse proxy
```

Four choices carry the design. Their reasons are in `docs/DECISIONS.md`
(decisions 35–40).

| Choice | In one line |
| --- | --- |
| Fragmented MP4 to the browser's MSE, not WebRTC and not go2rtc | The recorder already produces fMP4. MSE uses the existing port and login, plays H.265 where the browser can, and needs no second product inside ours |
| One new supervised process, `media` | Viewer load and viewer bugs stay out of the guardian and the workers |
| The browser stitches playback from byte ranges | The server stays stateless and cheap: no remuxing, no per-viewer state |
| Sub streams are connected by the media process | The live grid needs no change to the recorder at all |

What already exists and is reused:

- `worker.Fanout` (`internal/worker/fanout.go`): bounded, dropping, carries
  raw RTP. `Camera.Subscribe` is the hook for the restream.
- `processor` (`internal/worker/tracks.go`): RTP to samples for H.264, H.265,
  AAC, Opus and G.711/G.726. The live muxer needs exactly this.
- `mp4meta.AppendFragmentHeader`: the fragment writer, already zero-copy.
- The `mfra` index every closed file ends with (decision 3): the list of
  keyframe-aligned chunks, in one small read.
- `guardian.ProcSpec`: supervision of a new process is a struct.
- `Guardian.OpenSegment`: nearest-copy selection, with share credentials.
- The Live page (grid, layouts, saved views, single view) and the Recordings
  page, both waiting for video.
- The camera simulator and the "decode every file with ffmpeg" test habit.

## 2. Order of work

Five milestones. Each one ends in something the owner can use, and each is
shippable on its own. The order is by risk to recording: **the recorder
process is not touched until milestone 3**, and then only by one small,
switchable component.

| | Milestone | Owner sees | Touches the recorder? |
| --- | --- | --- | --- |
| M0 | Groundwork | Sub-stream field on the camera form; camera limits per user | No (one refactor, behaviour unchanged) |
| M1 | Live grid | Moving pictures on the Live page, from sub streams | No |
| M2 | Timeline playback | The Recordings page becomes a timeline that plays | No |
| M3 | Main stream live | Full-quality single view with audio; cameras without a sub stream go live | Yes: the loopback restream |
| M4 | Fast and wide | Speeds, scrubbing, thumbnails, four cameras in sync, the hour in progress | No |
| M5 | Proof | Acceptance runs of spec 16.11; operations guide | No |

### M0 — Groundwork

No viewer-visible video yet. Everything here is needed by more than one later
milestone.

1. **`internal/stream`: move, do not rewrite.** Move `processor`, `sample`,
   `videoParams`, `audioParams` and the audio handlers out of
   `internal/worker/tracks.go` and `segment.go` into a new package
   `internal/stream`, exported, with `Packet` and `Track` from `fanout.go`.
   The worker imports them. No behaviour change: the recorder tests
   (`go test ./internal/worker/...`, with ffmpeg installed) must pass
   untouched. This is the only edit to recorder code before M3.
2. **`winsvc.OpenShared(path)`**: open read-only with
   `FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE` through
   `windows.CreateFile` (`fs_windows.go`), plain `os.Open` in `fs_other.go`.
   Go's `os.Open` on Windows does not share delete, so a file held open by a
   reader cannot be renamed or deleted. Use it in `Guardian.OpenSegment` now:
   today a v1 download of a staging file can make the mover's delete of that
   file fail. Test on the Windows CI job: open with `OpenShared`, then rename
   and delete the file from another handle; both must succeed.
3. **Process priority in `ProcSpec`**: a `Priority` field, applied in
   `winsvc` when the child starts (`BELOW_NORMAL_PRIORITY_CLASS`); no-op
   elsewhere.
4. **Config schema 2** (`internal/config`): the `live` section
   (`enabled`, `mainStream`, `views`), validation (view ids unique, layout one
   of 1/4/9/16, camera ids exist), the first real migration
   `config_v1_to_v2`, and diffing so that a change to `live` restarts nothing
   in the recorder.
5. **Sub stream in the camera form and test.** `subStreamPath` in
   `CameraInput` (it is already stored and returned). `worker/probe.go` tests
   the sub stream too; `CameraTestResult` gains `sub`. A change to
   `subStreamPath` must not count in `recorderChanged`: it never restarts a
   recording.
6. **Per-user camera limits** (`internal/api/auth.go`, `users.json`): the
   `cameras` field; one function `allowed(user, cameraID)` used by every
   handler that names a camera, and a filter for lists, events and the SSE
   stream. Write the test first: a table of every `/api/v1` route against a
   restricted viewer, which fails for any route added later without a rule.
7. **Simulator** (`test/simulator`): a second, low-resolution path per camera
   (`/cam1/sub`), an H.265 sub stream switch, and a switch that refuses the
   sub stream.
8. **Check the H.265 sample entry.** Record an H.265 file and read its sample
   entry type. Safari only plays `hvc1`; Chrome and Edge play both. If the
   recorder writes `hev1`, the live muxer (which builds its own header) uses
   `hvc1` regardless; for recorded files, decide whether to change the
   recorder's tag (then re-verify VLC and Windows Media Player) or to accept
   that H.265 recordings do not play in Safari. Record the decision.

Exit: all v1 tests green on Linux and Windows; a v1 `config.json` loads and
is rewritten as schema 2; a restricted viewer is fenced in.

### M1 — Live grid from sub streams

1. **`buttercams media` skeleton** (`internal/media`, `cmd/buttercams`):
   reads `assign_media` from stdin, heartbeats, listens on `127.0.0.1:0`,
   reports `media_ready` with the port, exits when stdin closes. Every
   request must carry the run's token in a header; anything else gets 403.
2. **Guardian** (`internal/guardian/media.go`): a `ProcSpec` for the media
   process (heartbeat limit 15 s, below-normal priority, restart above 1 GB),
   started when `live.enabled`; sends cameras and tokens on start and on every
   config change; keeps the port; adds `media` to the health check as a
   Warning when down for 2 minutes (never Critical: recording is unaffected).
   Sub-stream passwords are sent as stored (`passwordEnc`) and unprotected in
   the media process, as workers do.
3. **Reverse proxy** (`internal/api/media.go`): `guard(accessViewer, …)`,
   camera permission, `Origin` check for WebSockets, then
   `httputil.ReverseProxy` to the media port with the token header. Not
   wrapped in the 30 s timeout handler (like `/stream` and downloads). Dial
   timeout 2 s, then `503 media_unavailable`. A semaphore caps media
   connections at 200.
4. **Stream hub** (`internal/media/hub.go`): `source` per camera and stream,
   reference-counted, 30 s linger. A sub source is a gortsplib client with
   the worker's connect timeout, 10 s stall rule and backoff, feeding
   `stream.processor`. The hub keeps the samples since the last keyframe.
5. **Live muxer** (`internal/media/live.go`): initialization segment from the
   parameter sets (as `openSegment` does), then one `moof`+`mdat` per video
   frame with `mp4meta.AppendFragmentHeader`. The MIME string with codecs
   (`avc1.PPCCLL`, `hvc1.…`, `mp4a.40.2`, `opus`) is computed from the
   parameter sets on the server, so the browser parses nothing.
6. **WebSocket handler** (`internal/media/ws.go`): `init` text message,
   initialization segment, the cached group of pictures, then live fragments.
   Per-viewer bounded queue (2 s or 4 MB); on overflow, empty it and resume at
   the next keyframe. Write deadline 10 s. Use `gorilla/websocket`, already in
   the module graph.
7. **Web player** (`web/src/player/LivePlayer.ts`, used by `LivePage.tsx`):
   - `MediaSource` or `ManagedMediaSource`, one `SourceBuffer` in `segments`
     mode; an append queue driven by `updateend`.
   - Live edge control every 500 ms: more than 1 s behind → `playbackRate`
     1.1; more than 3 s → seek to the end minus 0.2 s; remove buffered video
     older than 10 s.
   - Reconnect with backoff (1, 2, 5, 10 s) and keep showing the v1 state
     tile underneath until the first frame arrives.
   - Close the socket when the tile is not visible (`IntersectionObserver`,
     Page Visibility), reopen when it is.
   - Check `isTypeSupported(mime)` on `init`; if not supported, say so in the
     tile.
8. **Saved views on the server**: `GET`/`PUT /live/views`; the page offers
   once to upload views found in `localStorage` (decision 28b).
9. **Dashboard**: a row for the media process (state, viewers, sources).

Tests:

- Go: a test viewer connects to the WebSocket through the real proxy, writes
  what it receives to a file and decodes it with ffmpeg: frames continuous,
  timestamps monotonic, first frame a keyframe. Variants: H.264, H.265, 1 s
  and 4 s keyframe interval, camera stall and return, parameter change.
- Go: a viewer that never reads does not delay a second viewer (overflow
  path), and the source closes 30 s after the last viewer.
- Go: kill the media process during a stream; a new stream works within 10 s;
  the recorder's files in the same test have no gap.
- Browser (Playwright, see section 4): 16 tiles play; `currentTime` advances
  on each; latency (wall clock burnt into the simulator's picture, or the
  buffered end against the clock) stays under 1 s for 10 minutes.

Exit: Garrett sees his cameras' sub streams on the Live page.

### M2 — Timeline playback

1. **`GET /timeline`** (`internal/api`, `internal/index`): segments of one
   camera overlapping `[from, to)`, including the open one (`end: null`), and
   that camera's recorder events in the range. One indexed query each
   (`idx_segments_camera_start`, `idx_events_ts`). Range limited to 31 days.
2. **Path resolution for the media process.** Split `Guardian.OpenSegment`
   into "candidates in order" and "open". The proxy handler for
   `/segments/{id}/play/*` resolves the candidates (and connects the share
   when it has credentials, as today), and passes them in a header to the
   media process, which tries them in order with a 3 s limit each.
3. **Playback file server** (`internal/media/playback.go`):
   - `mp4meta.ReadIndex(r, size)`: the `tfra` entries (time, `moof` offset),
     the initialization segment's length, the trailer's duration. A chunk is
     the bytes between two consecutive entries; the last one ends at the
     trailer. Unit test: for every recorder test file, the chunks cover the
     file from the end of the header to the trailer exactly once.
   - `/play/index`: JSON `{mime, init: [0, n], start, chunks: [[startMs,
     durationMs, offset, size], …], open}`. Cached in an LRU (256 files) keyed
     by path, size and modification time.
   - `/play/data`: `http.ServeContent` on a file opened with
     `winsvc.OpenShared`. Handles are pooled and closed after 30 s idle.
     `Cache-Control: private, max-age=3600` for closed files.
   - Read budget: a token bucket (40 MB/s) around reads from the staging
     volume.
4. **Web player** (`web/src/player/TimelinePlayer.ts`):
   - A **timeline map**: for each file an entry `{mediaStart, wallStart,
     duration}`. A file that follows the previous one without a gap
     (`gapBefore` false and starts within 1 s of the previous end) is placed
     at `previous.mediaStart + previous.duration`: joined by exact media
     duration, so the `SourceBuffer` sees no hole. Otherwise it is placed
     after a deliberate jump, and the player skips it and shows the gap.
   - Appending: set `timestampOffset` to the file's `mediaStart`, append its
     initialization segment, then chunks in order. Keep 30 s buffered ahead
     and 30 s behind; fetch the next file's index when 60 s from its start.
   - Seeking: abort, find the chunk containing the time, remove the buffer,
     append from that chunk, set `currentTime`.
   - A different MIME string or track set in the next file: finish the
     current `MediaSource`, start a new one at the boundary.
   - The rollover overlap (spec 6.5) is cut with `appendWindowEnd`.
5. **Timeline component** (`web/src/pages/recordings/Timeline.tsx`): canvas
   or SVG track, zoom and pan, spans, gaps, event marks, playhead, keyboard
   (space, arrows, `,` and `.` for frame steps later). Day with 23 or 25
   hours laid out from the segments' offsets, not from `24 * 60`.
6. The old table becomes the **Files** tab unchanged.

Tests:

- Go: the stitch rule in a test client: indexes and ranges of three
  consecutive hour-rollover files (the injectable clock of decision 33)
  concatenated with the offsets the player would use, decoded by ffmpeg, no
  missing or repeated frame across the two seams.
- Go, Windows CI: while a client holds `/play/data` of a `.part.mp4` open,
  the worker closes and renames it; while a client reads a verified staging
  file, the mover deletes it; retention deletes a file being read. All three
  succeed and the client gets a clean end of stream.
- Go: a share that hangs on open answers within the limit and falls back to
  the next copy.
- Browser: play across a seam, seek into a gap, seek to another day.

Exit: any moment of any recorded day plays in the browser, across hours.

### M3 — Main stream live through the restream

This is the only milestone that adds code to the recorder process. Keep it
small and review it against spec 16.9 line by line.

1. **`internal/restream`**: a gortsplib `Server` on `127.0.0.1:0`, started
   by the worker when the guardian sends `restream` with a token. Path
   `/<camera-id>`. On `DESCRIBE`, the mount is created from the camera's
   current session description; a camera that is not recording answers 404.
   On `PLAY`, `Camera.Subscribe("restream", 1024)` and a pump goroutine copies
   from the reader's channel to the server stream. On the last client's
   teardown: `Unsubscribe`. A second client for the same camera: 503.
2. The pump watches `Packet.Session`: when it changes, it closes the mount.
   The media process sees the stream end and connects again.
3. Every restream goroutine starts with `defer recover()`; on a panic the
   mount is closed, an event is emitted and that camera's restream stays off
   until its next session.
4. `camera_stats` gains `restreamDropped` (from `Reader.Dropped()`).
5. The worker reports `restream_ready` with the port; the guardian passes
   worker addresses to the media process in `assign_media` and again whenever
   a worker restarts.
6. **Hub**: a main source is a gortsplib client to the restream. It needs no
   backoff logic of its own beyond "retry every second while a viewer waits".
7. **Audio** in single view: pass AAC and Opus through; G.711/G.726 through
   `audio.Transcoder` in the media process, created only for sources that have
   a viewer with `audio=1`.
8. **Live page**: single view connects to `main`; tiles of cameras without a
   sub stream use `main`; H.265 falls back to `sub` when the browser cannot
   play it.

Tests:

- Unit (`internal/worker`): with a restream client attached that never reads,
  the file written is byte-identical in content and timing to a run without
  the restream (alongside `TestFanoutDropsWhenReaderIsFull`), and `restreamDropped` rises.
- Integration: hourly rollover with a main-stream viewer attached: no gap in
  the files (the existing rollover test with a viewer added), and the viewer
  resumes within two keyframe intervals.
- Integration: single-session camera with a viewer: the camera sees one
  session only.
- Chaos: kill the media process, and separately make the restream pump
  panic (test hook), 100 times while recording; files are complete.
- Benchmark: 60 cameras, all with a restream client, against the figures of
  decision 6. The recorder's CPU must not move outside measurement noise;
  the copy into the server's write queue is paid by the restream goroutine,
  not the camera goroutine.

Exit: single view shows the main stream with audio; `live.mainStream: false`
removes every trace of the restream from a running worker.

### M4 — Fast and wide

1. **Keyframe endpoints** (`internal/media/playback.go`): for a chunk, read
   the start of its `moof` to find the first sample's size (`parseMoof`
   already exists), read that sample, and write a new `moof`+`mdat` holding
   only it, with the chunk's base time and the chunk's whole duration.
   `/play/keyframes?from=&to=` returns up to 120 of them concatenated;
   `/play/thumb?t=` returns the initialization segment plus one of them, a
   complete MP4, with `Cache-Control: private, max-age=86400`.
2. **Speeds**: `playbackRate` for 0.5× to 4×; above that, try `playbackRate`
   and watch `getVideoPlaybackQuality()`: if more than a quarter of frames
   drop over 2 s, switch to keyframe mode (append from `/play/keyframes`).
   Audio muted outside 0.5× to 2×.
3. **Scrubbing**: while dragging, show thumbnails; on release, seek.
4. **Frame step**: paused, `currentTime ± 1/fps` (fps from the index: samples
   per chunk are not in it, so take the frame duration from
   `requestVideoFrameCallback` where available, else from the camera's
   reported fps).
5. **Thumbnails**: a hidden `<video src="…/play/thumb?t=…">`, draw to a
   160 px canvas on `loadeddata`, keep 500 pictures in memory. One request in
   flight per camera, the latest hover position wins.
6. **The hour in progress**: `mp4meta.ScanFragments(r, from)` walks box
   headers from a known offset; the index of an open file is extended on each
   request and served with `open: true`. The player re-requests it every 2 s
   while within 30 s of its end, and moves on to the next file when the
   segment appears closed in `/timeline`.
7. **Four cameras in sync** (`web/src/player/SyncGroup.ts`): one wall clock
   driven by the first player; the others are corrected every 250 ms: off by
   more than 150 ms → rate ±5%; more than 1 s → seek. Buffering in one pauses
   the clock for at most 2 s, then the others continue and it catches up.

Tests: keyframe fragments decode with ffmpeg to exactly one picture per
chunk at the right times; the open-file index never lists a fragment the
writer has not finished (compare against the file after close); browser test
of 16× over two hours of footage and of four players within 150 ms.

### M5 — Proof

- The acceptance list of spec 16.11, on the target Windows Server where it
  says so: 8-hour 16-tile run, 20 viewers (headless browsers plus the Go test
  viewer), kill and CPU-pin runs from `test/chaos`, both clock-change days,
  the four browsers, Tailscale.
- Recorder CPU and memory with and without viewers, added to the table of
  decision 6.
- `docs/OPERATIONS.md`: watching live and recordings, setting a sub stream on
  common camera brands, browser requirements, remote access over Tailscale or
  WireGuard, what "Live view is restarting" means. `docs/API.md`: the new
  endpoints as built. `README.md`: v2 implemented.

## 3. Risks, and what answers each

| Risk | Answer |
| --- | --- |
| The restream slows or crashes a worker | Built last; on demand; one client; drop-only queue; `recover()` everywhere; off switch; byte-identical-file test and benchmark gate (M3) |
| A viewer's open file blocks the rollover rename, staging cleanup or retention on Windows | `winsvc.OpenShared` everywhere, handle pool with idle close, Windows CI tests (M0, M2) |
| Playback reads starve recorder writes on the staging disk | Read budget in the media process; below-normal priority |
| Delay creeps up in a tile left open all day | Live edge control and buffer trimming in the player; the 8-hour test |
| A browser cannot play H.265 | Detect with `isTypeSupported`; fall back to an H.264 sub stream live; say so and offer the download for recordings; recommend H.264 sub streams in the camera test |
| Safari and `hev1` | Checked in M0, step 8 |
| iPhones before iOS 17.1 have no MSE | Not supported in v2; listed in the operations guide. An HLS fallback is in `docs/IDEAS.md` |
| Adjacent files do not line up exactly on the clock | Files without a gap are joined by media duration, not by wall clock (M2, step 4) |
| A file's tracks differ from the previous one (audio appears, resolution changes) | New `MediaSource` at the boundary |
| Sub streams use up a camera's session limit | One sub session per camera, shared by all viewers, closed 30 s after the last one |
| The media library changes its box layout | The live muxer and index reader use `mp4meta`, which ButterCams owns and tests against ffmpeg |
| Guardian load from proxying | Byte copy only, connection cap; measured in M5. If it shows, the media process can take the HTTPS listener for `/live` and `/play` paths later without changing the API |

## 4. Test tooling to add

- **Go test viewer** (`internal/media/mediatest`): WebSocket and playback
  client used by the integration tests above; it writes what a browser would
  append, for ffmpeg to decode.
- **Browser tests** (`web/e2e`, Playwright). The Chromium that Playwright
  downloads has no H.264 or AAC decoder, so these tests must run in an
  installed browser: `channel: 'chrome'` or `'msedge'`. On the Windows CI
  runner Edge is present. Run them against `buttercams console` plus the
  simulator. They are a separate CI job so the Go tests stay at about two
  minutes.
- **Simulator**: sub paths (M0), and a wall clock burnt into the picture or
  carried in an SEI message so latency can be measured from outside.
- **Chaos script**: kill and CPU-pin the media process; viewers that connect
  and vanish at random.

## 5. Questions for the owner

None of these block M0.

1. Do Garrett's cameras have a sub stream enabled, and can it be set to
   H.264? The grid depends on it.
2. Which browsers and devices will be used to watch? (Older iPhones are the
   one known gap.)
3. Is a short freeze of the full-quality single view at the top of each hour
   acceptable? Removing it means switching sessions inside one restream
   mount, which is more code inside the worker.
4. Are shared saved views right, or should each user have their own?
