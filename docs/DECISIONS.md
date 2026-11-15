# Decisions made while building ButterCams

The build specification (`BUTTERCAMS_SPEC.md`) is the source of truth. Where
it left a choice open, was ambiguous, or could not be followed to the letter,
the choice made is recorded here with its reason. When in doubt, the option
that best protects continuous recording was taken (spec, preamble).

Numbers quoted below were measured on the development machine (a 4-vCPU
Linux VM) with the camera simulator; they are not a substitute for the 72-hour
and 30-day runs on the target server (spec 14.3).

## Media

### 1. Audio conversion uses a pure-Go AAC encoder inside the worker

The spec asks for a pure-Go G.711 decoder plus AAC encoder, with a bundled
`ffmpeg.exe` as the fallback "if no reliable pure-Go AAC encoder exists".

**Choice:** `github.com/tphakala/go-aac` (a Go port of FFmpeg's AAC-LC
encoder), running inside the recorder worker. No `ffmpeg.exe` is bundled.

**Why:** it keeps ButterCams a single executable, and an in-process encoder
costs far less memory than one ffmpeg process per camera (sixty processes
would have put the 1.5 GB memory target at risk). Its output was checked
against an independent decoder: the tests decode it with ffmpeg and verify
content and timing, and the presentation times were confirmed to line up
(encoder delay of exactly one 1024-sample frame, accounted for in
`internal/audio`). A fault in the encoder cannot stop video: audio processing
is wrapped in panic recovery and any audio error drops audio for that
connection only.

**Things to know:**

- **Sample rate.** The encoder only supports 44.1 and 48 kHz, so camera audio
  (8 kHz, sometimes 16 kHz) is upsampled to **48 kHz** before encoding at
  32 kbit/s mono. The spec says "16 kHz … (or a sample rate matching the
  source)"; 48 kHz is a deviation forced by the library. The coded bandwidth
  is still limited to the source's (4 kHz for G.711), so no bits are wasted.
- **Encoder mode.** The "fast" quantizer search is used: about 0.5% of one
  CPU core per camera instead of 1.1%, for roughly 2 dB less signal-to-noise
  on telephone-band audio. One constant in `internal/audio/transcoder.go`
  (`aac.CoderFast`) switches it back.
- **License.** go-aac is **LGPL-2.1-or-later** because it derives from
  FFmpeg; the rest of ButterCams is MIT. That is fine while ButterCams'
  source is available to whoever receives the binary (they can rebuild with a
  changed go-aac). If ButterCams is ever shipped closed-source this must be
  revisited. See `THIRD_PARTY_NOTICES.md`.
- **Maturity.** The library is young (v0.7). If it proves unreliable on
  Garrett's cameras, the swap is contained: `internal/audio.Transcoder` is
  the only user, and the spec's ffmpeg side process can replace it there.

### 2. G.726 decoding follows the ITU standard, not FFmpeg, at 40 kbit/s

The G.726 decoder (`internal/audio/g726.go`) implements the ITU-T algorithm.
It is tested against FFmpeg's decoder for 16, 24, 32 and 40 kbit/s in both
bit packings. FFmpeg deviates from the standard in one constant at 40 kbit/s
(it leaks the zero predictor by 2^-8 where the standard says 2^-9); the test
sets that constant to FFmpeg's value to compare everything else, and the
shipped decoder uses the standard's value, which is what cameras implement.

### 3. Recording metadata is stored as standard tags, plus a close marker

The spec asks for the start time and camera in an MP4 `udta` box
(`buttercams:start`, `buttercams:camera`).

- They are written as iTunes-style free-form tags under
  `moov/udta/meta/ilst`, the form ffprobe, VLC and ExifTool display. A third
  tag, `buttercams:gap` (`0`/`1`), records whether the file started after an
  interruption.
- When a file is closed, a 128-byte `free` box (`buttercams:close=<reason>;
  dur=<ticks>`) and a standard `mfra` random-access index are appended. Both
  are ordinary boxes that players skip or use (VLC uses `mfra` for seeking).

**Why:** with these, the index can be rebuilt from the files alone with three
small reads per file instead of walking 3,600 fragments each, which is the
difference between minutes and hours over a network share. A file without
the marker is by definition one that was not closed cleanly.

### 4. A recording's end time is its start plus its video duration

The index stores `end = start + video duration`, to the millisecond, both
when a worker closes a file and when the index is rebuilt. That is what makes
"deleting the database and rebuilding gives an identical segment list" true.
`start` is the wall-clock time the file's first keyframe was received.

**Not restored by a rebuild:** the SHA-256 (a file cannot contain its own
hash). It is recomputed the next time the file is copied, and the copy is
still verified by reading it back. `protected` is always false in v1.

### 5. What counts as a keyframe

H.264: an IDR picture, or a picture following a recovery-point SEI (some
"smart codec" cameras never send IDRs after the first). H.265: IDR and CRA
pictures. Files and fragments start only on these.

### 6. The fragment writer

The initialization segment (`ftyp` + `moov`) is produced by the mediacommon
fMP4 package as the spec prescribes. The `moof` headers of the fragments are
written by ButterCams itself (`mp4meta.AppendFragmentHeader`, same layout as
mediacommon's), so that video data goes from the network buffers into the
file with one copy and no per-fragment allocation. With the library's
fragment writer the same data was copied three times, which cost about 40% of
the recorder's CPU. Measured for 60 simulated cameras at 4 Mbit/s each:

| | CPU, all processes | Memory, all processes |
| --- | --- | --- |
| Video only | 34% of one core (4.2% of an 8-core CPU) | 330 MB |
| Plus G.711 → AAC on all 60 | 71% of one core (8.9% of an 8-core CPU) | 420 MB |

### 7. UDP transport

With `transport: udp`, lost packets damage the pictures up to the next
keyframe. They are recorded as received; nothing is concealed or repaired.
TCP is the default for that reason.

## Recording behaviour

### 8. A feed refresh that succeeds late starts a `_b` file without a gap

When the second session is not ready by HH:00:15, the old session starts the
new hour's file (spec 6.5 step 4) and the refresh is retried at HH:05:00 and
every five minutes. When a retry succeeds mid-hour, recording moves to the
new session in a new `_b` file with `gap=false` and close reason
`hour_rollover`. The spec does not say what the file should be; a new file is
the safe choice because the two sessions have unrelated timestamps.

### 9. Single-session cameras

A camera is treated as single-session when the second session is refused
(connection refused, RTSP 453 or 503) **or** when the camera closes the first
session within twenty seconds of the second one starting to play. In the
second case recording continues on the new session with the gap marked.

### 10. Clock changes are detected per camera

Each camera compares wall-clock and monotonic time at every keyframe and four
times a second. A jump of more than five seconds closes the file at the next
keyframe with reason `clock_jump`, even when the jump crosses an hour
boundary (which would otherwise look like an ordinary rollover).

### 11. Recording names are never reused

A camera picks the first free name for the hour (`…`, `_b`, `_c` …) by
looking at the staging folder and at the names it used itself. Because files
leave staging once copied, the guardian also sends each worker the names in
the index from the last three hours whenever it (re)starts the worker. As a
last line of defence, a finished file whose name is already indexed is
renamed to the next free name before it is indexed. The integration tests
found the case this protects against: worker crash after the earlier file of
the same hour had already been copied and removed from staging.

### 12. State changes are reported at once

Workers send `camera_stats` every five seconds as specified, and additionally
at every state change, so the dashboard and the health check see a camera
leave the recording state within a second (rule 6).

### 13. The packet fan-out carries RTP packets

The fan-out of spec 15 carries the raw RTP packets with their timing; the
file writer (the only reader in v1) depacketizes them itself. This is what
the restream (spec 16.3) needs, so nothing has to change in the recorder later. The
file writer's queue holds 4,096 packets; if it ever overflows (a disk that
stopped), the dropped packets are counted, a warning is raised, and the
writer resynchronizes at the next keyframe.

## Storage

### 14. Copies are verified by reading them back

After copying, the mover reads the file back from the destination and
compares size and SHA-256 with what the recorder computed while writing. This
doubles the read load on the destination, but the staging copy is deleted
after verification, so the check has to be a real one.

### 15. Who decides and who deletes

The mover never opens the database (spec 4.1), so retention is split: the
mover measures free space (every five minutes and after each verified copy)
and reports it; the guardian, which owns the index, selects what to delete
(`internal/mover/retention.go`, pure functions with unit tests) and sends the
list; the mover deletes exactly those files and reports back. Only names
matching the ButterCams pattern are ever acted on.

**Disk-full rule.** "Act as if the cap were hit" (below 2% free) is
implemented as: treat the current usage as the cap, so the oldest 5% of what
ButterCams stores there is deleted.

### 16. Staging copies are removed as soon as every destination has the file

Not at the end of the hour. Holding them longer would keep up to an hour of
every camera in staging (about 100 GB for sixty 4 Mbit/s cameras) for no
benefit, since names are protected by decision 11.

### 17. Disabled and removed destinations

No copies are queued for a disabled destination, and disabling one cancels
its waiting copies, so staging is not held up by a destination nobody is
using. Saving any storage setting makes the mover retry failing destinations
at once instead of waiting out the retry schedule: someone probably just
fixed something. Removing a destination from the configuration forgets its copies in
the index; **the files on it are left alone**.

### 18. Changing the staging folder

Files recorded before the change stay where they are until they are copied or
removed. The previous folders are remembered in the database (`meta` table)
so those files can still be found, copied, downloaded and cleaned up.

### 19. Deleting a camera with its recordings

With "keep recordings" unticked, every indexed recording of the camera is
deleted from staging and from every destination; files of that camera that
finish copying in the following ten minutes are removed too. Nothing that is
not in the index is touched.

### 20. Index rebuild reconciles instead of replacing

`rebuild-index` (command and System page button) brings the index in line
with the files on disk. On an empty database that recreates everything. On a
live one it is safe while recording continues: files being written and rows
changed after the scan began are left alone, copies still waiting stay
queued, and what the index says about a destination that could not be
reached is kept. Staging files that still belong on a destination are queued
again, which restores the copy queue after the database was lost. The index
is also rebuilt automatically when the database file is missing or fails
`PRAGMA quick_check` at start-up.

Rows of recordings that no longer exist anywhere are removed from the index a
day later, so the database does not grow without bound.

The database has the tables and indexes of spec 10.5 plus one more index,
`idx_copies_destination_state`, which keeps the per-destination usage, queue
and retention queries fast once there are hundreds of thousands of rows.

### 34. Copies that fail together count as one failed attempt

Up to four copies run at once. When a destination goes away they all fail
within moments of each other. That is one failed attempt for the retry
schedule of spec 7.3 (30 s, 1, 2, 5, 10 min), not four: otherwise a short
outage would jump straight to the five-minute wait. Each of those copies
still reports its failure, with the same attempt number.

## Guardian

### 21. Children are put in the Job Object right after they start

The guardian creates a kill-on-close Job Object and assigns each child to it
immediately after starting it; the guardian itself stays outside. (Putting
the guardian inside would terminate it when the handle is closed at shutdown,
which the service manager would treat as a crash.) The few microseconds
between start and assignment are covered by a second mechanism: a child
exits by itself when its stdin closes, which happens when the guardian dies.

### 22. Worker assignment

Cameras are assigned in configuration order, six per worker, at start-up.
While running, assignments never move: a new camera takes the first free
slot, a deleted one leaves a free slot, and a worker left without cameras is
stopped. So a change only ever touches one worker.

### 23. Health check details

- The first check runs ten seconds after start, then every sixty seconds.
- A failing destination raises a Warning once its queue is two hours old
  (check 5 of spec 10.1). Before that it shows as `retrying` on the
  dashboard and in the Events list without changing the overall status.
- The on-disk watchdog (10.2) acts in two steps: the first time a recording
  file has not grown, the camera is reconnected; if it still has not grown at
  the next check, its worker is restarted.
- A camera whose worker stopped reporting counts as not recording.

### 24. Configuration recovered from a backup

When `config.json` is unreadable, the newest readable backup is used, the
unreadable file is kept as `config.json.unreadable-<time>`, and the status is
Critical until an administrator saves the settings once (any change), which
confirms them.

## Web UI and API

### 25. Where accounts live

User accounts are stored in `<dataRoot>\users.json` (argon2id hashes), not in
the database and not in `config.json`: the database can be deleted and
rebuilt from the recordings, and a configuration export must not carry
logins. Sessions are kept in memory; a service restart signs everyone out.

Password hashing uses argon2id with OWASP's minimum parameters (19 MiB, 2
passes) and runs one hash at a time, so a burst of login attempts cannot take
resources from recording. Passwords need at least eight characters.

### 26. Additions to `config.json`

`logging.retentionDays` (default 14) was added, because the System page must
be able to change log retention and the v1 schema had no place for it.

### 27. API additions

Needed by the pages of spec 11.2 and not in the table of 11.3:
`GET /auth/session`, `GET|POST|PUT|DELETE /users`, `POST /system/certificate`,
`GET /system/config/export`, `POST /system/config/import`,
`POST /cameras/import`, `GET /recordings/cameras`. The full contract is in
`docs/API.md`.

Camera statistics include `lastPacketAgeSec` and `reportAgeSec`, measured on
the server, so the dashboard shows correct ages even when the viewing PC's
clock is wrong.

### 28. Changing the port or address

The listener moves about one second after the request was answered. If the
new address cannot be used (port taken), the previous one is restored and a
Critical event is recorded. A new certificate takes effect immediately
without restarting the listener.

### 28a. Look of the settings page

Black, white and butter yellow; orange, red and blue only mark states, and
every state also has a text label. Headings use Bricolage Grotesque, text
Instrument Sans and paths and numbers Martian Mono (all SIL OFL). The fonts
come from the `@fontsource` npm packages and are bundled into `web/dist`, so
the page loads nothing from the internet (spec section 3).

### 28b. Live page before live video

The owner asked for the Live page now. It shows the live view grid (layouts
1, 4, 9 and 16, saved camera views, click a tile for one camera), but without
video: the live engine and the local restream do not exist yet, so each
tile shows the camera's recording state from `GET /cameras`, refreshed every
5 s, and says plainly that live video is not in this version. Saved views are
kept in the browser's `localStorage` (one list per browser), so v1 needs no
change to `config.json` or the API. When v2 adds video (spec 16), the views
move to the server and the tiles play the sub stream.

## Installer and tooling

### 29. Inno Setup instead of WiX

The spec prefers a WiX v4 MSI and allows Inno Setup. The installer needs a
Test button for the destination, disk-space and system-drive warnings,
account selection with "Log on as a service", and a keep-or-remove question
at uninstall. In an MSI each of these needs a native custom-action DLL; in
Inno Setup they are a few lines of script that call `buttercams.exe`. All
system changes are done by `buttercams.exe setup …` sub-commands (written in
Go, exercised on a real Windows machine in CI); the wizard only collects
answers. Passwords are passed to those commands through environment
variables, not on the command line.

### 30. `setup` sub-commands

`buttercams.exe setup init | install-service | uninstall-service |
test-destination` are not in the spec's list of sub-commands. They exist so
the installer has no logic of its own.

### 31. Development on Linux and macOS

Everything except the Windows integration (service manager, Job Objects,
Event Log, DPAPI, WNet, registry, firewall) also builds and runs on Linux and
macOS, with small stand-ins in `internal/winsvc/*_other.go`. This is for
development and CI only. **On those systems passwords in `config.json` are
not encrypted** (there is no DPAPI) and the guardian says so in its log.

### 32. Camera simulator

`test/simulator` is a small Go RTSP server built on gortsplib (the spec
allows MediaMTX or a Go server). It serves embedded sample streams and has
switches for every case in spec 14.1, plus hooks the tests use to stall a
stream, drop its sessions or refuse new ones.

### 33. Testing the hourly rollover

The hour length is a constant (`naming.SegmentDuration`), as required. The
worker's wall clock is injectable in tests only, so the rollover tests start
"thirteen seconds before the hour" instead of waiting for one.

## Roadmap and v2 design

These were decided when live view and timeline playback were made the next
phase. Nothing in this section is built yet; `docs/V2_PLAN.md` has the build
order.

### 35. Live view and timeline playback are v2

The spec planned them last (v4), on top of the extension host, the restream
and the export process of the alerts-and-motion version. The owner wants to
watch cameras before anything else, and the Live page already exists without
video (decision 28b). The versions are now: v2 live view and timeline
playback (spec 16), v3 alerts, clip export and motion detection (17), v4 AI
object detection (18). Section numbers 16–18 changed with them.

What moved with it: browsing and in-browser preview of recordings (old "clip
access") are part of the v2 timeline. What stayed behind: trimmed exports and
protection (v3), event search (v3, with the first events worth searching),
AI filters (v4). v2 needs nothing from v3 or v4.

### 36. Fragmented MP4 over MSE instead of go2rtc and WebRTC

The spec named go2rtc with WebRTC and an MSE/HLS fallback.

**Choice:** the media process sends fragmented MP4 over a WebSocket (live)
and byte ranges over HTTP (playback); the browser plays both through Media
Source Extensions. No go2rtc, no WebRTC.

**Why:**

- The recorder already depacketizes RTP and writes fMP4 with code ButterCams
  owns. Live view is that muxer with one fragment per frame.
- One port, one certificate, one login. WebRTC needs UDP ports, a firewall
  rule and address negotiation, and does not pass through a plain HTTPS
  tunnel; go2rtc brings its own web server and API, which would have to be
  hidden behind ours and know nothing of our users or camera permissions.
- H.265 plays through MSE wherever the browser can decode it. Over WebRTC it
  mostly does not.
- Timeline playback needs MSE anyway, so the page has one kind of player.
- Latency of 0.3–0.8 s on a LAN meets the target of under 1 s.

**Cost:** iPhones before iOS 17.1 have no MSE and cannot watch. WebRTC stays
possible later (`docs/IDEAS.md`).

### 37. A separate media process

Live and playback serving could have lived in the guardian, which already
serves downloads. It is its own supervised process (`buttercams.exe media`)
instead: the guardian supervises everything else and must stay small and
idle, and a viewer-facing component is where load spikes and parsing bugs
are most likely. The guardian keeps the login, the permission check and the
index queries, and forwards the bytes.

### 38. The media process connects to sub streams itself

The spec had each worker connect to its cameras' sub streams and re-serve
them. Instead the media process opens sub streams directly, on demand. The
recorder process then has nothing to do with sub streams: a sub stream that
stalls, floods or crashes its reader cannot touch a recording, and the live
grid needs no change to the worker. Main streams still come only through the
worker's loopback restream, because a second main-stream connection could
take the session the hourly rollover needs. From v3, extensions read both
from the media process (spec 17.2).

### 39. The browser assembles playback; the server keeps no state

The spec said the server "serves a continuous fMP4 stream across segments".
Instead the browser fetches each file's index and byte ranges and joins them
in its own buffer. Seeking, speed changes and several viewers then cost the
server only range reads, and there is no per-viewer session to leak or to
lose when the media process restarts. The result on screen is the same:
playback across hour files without a seam.

### 40. Thumbnails are decoded in the browser

"Generated from keyframes on demand and cached" would need a video decoder
on the server, which rule 2 and the no-CGO rule exclude. The server cuts one
keyframe out of the file as a tiny complete MP4; the browser decodes it and
caches the picture.

### 41. Saved views: each user's own, plus shared ones from Admins

The owner's answer. A user's own views are stored with the account in
`users.json`; shared views are stored in `config.json` (`live.views`), so a
configuration export carries the shared ones and no personal ones. "Share"
copies an Admin's view into the shared list; the copies are independent
afterwards.

### 42. The hourly rollover does not freeze live view

The owner asked to avoid the freeze if possible. The worker's restream serves
each RTSP session as its own stream and reports when a camera's second
session starts; the media process connects to both and switches viewers at
the new session's first keyframe. The switch logic is in the media process,
so the worker gains one message and a session filter, nothing that looks at
video. When the notice is missing, live view resumes at the next keyframe as
it would after any reconnect.

### 43. Browsers, phones and `hvc1`

The owner watches in Chrome and Safari, on Windows, iPhone and Android. Those
are the acceptance targets; Edge and Firefox are expected to work. iPhones
need iOS 17.1 (the first with `ManagedMediaSource`); older ones get a message
(decision 36). Because Safari plays H.265 only with the `hvc1` sample entry,
H.265 recordings are written that way from v2 on. Phone-size screens show at
most four live tiles per page.

## Releases

### 44. Releases use the shared release infrastructure

The owner asked for a Windows executable and a release pipeline built the
way JX-Holdings-LLC/release-infrastructure does it. A release is started by
hand and carries the installer, `buttercams.exe` on its own (it was only
inside the installer before) and a checksum file. The toolkit's defaults are
kept: tags `v<version>`, the `standard` security profile, and a version
advance with a changelog section for every change to `main`. The first
version is 1.0.0, the number the installer already used. `version.go` keeps
a `-dev` suffix so a build that did not come from CI or a release says so.
The service smoke test moved from the CI workflow to `installer\smoke.ps1`
so the release build runs the same one. Nothing is code-signed (no
certificate), and there is no self-update: spec section 3 rules out cloud
services, and an upgrade is running the new installer. See
`docs/RELEASING.md`.

## Not verified here

The build environment was Linux without a Windows machine. These parts are
compiled and vetted for Windows and exercised by the Windows CI job, but were
not run by hand on a server: the installer wizard, Windows Media Player
playback, UNC shares with credentials, DPAPI, the Event Log, and the
long-running acceptance criteria of spec 14.3 (72 h with 60 cameras, 30-day
soak, Garrett's real cameras, DST on a real system clock).

The Release workflow has not been run: the version checks, the Windows
cross-build and the collection and verification of the release files were
run locally with the toolkit, but the hosted run (Inno Setup, smoke test,
draft, publication) happens for the first time when it is dispatched. Use
the dry run first.
