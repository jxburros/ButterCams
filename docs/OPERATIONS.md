# ButterCams operations guide

ButterCams is a Windows service that records RTSP cameras continuously into
one-hour MP4 files, copies them to the storage you choose, and deletes the
oldest files when a destination reaches its limit. It is managed from a web
page. This guide covers installing it, setting it up, and what to do when
something looks wrong.

## 1. Install

Download `ButterCams-<version>-setup.exe` from the
[releases page](https://github.com/jxburros/ButterCams/releases). It is not
code-signed, so Windows SmartScreen may ask for confirmation; the release's
`SHA256SUMS.txt` lets you check the download
(`Get-FileHash ButterCams-<version>-setup.exe`).

Run it as an administrator on the server
(Windows Server 2019, 2022 or 2025; Windows 10/11 for testing). The wizard
asks for:

| Question | Notes |
| --- | --- |
| Data folder | Settings, index, logs and certificates. Default `C:\ProgramData\ButterCams` |
| Staging folder | Every camera records here first. Use a **local** disk with room to spare, ideally not the Windows drive. The wizard warns below 100 GB free |
| First destination (optional) | A local or attached disk, or a `\\server\share` path with optional credentials. **Test** checks it. You can skip this and add destinations later |
| Web port and administrator | Default port 8650. The administrator name and password are what you sign in with |
| Service account | Local System, or an account of yours. Use your own account when writing to shares that have no credentials of their own; it is given "Log on as a service" |

The installer then registers the service **ButterCams Recorder** (Automatic,
Delayed Start; restarted after 5 seconds on any failure), opens the web port
in Windows Firewall for Domain and Private networks, creates a self-signed
HTTPS certificate, adds the Start-menu shortcut **ButterCams Settings**, and
starts the service.

## 2. Open the settings page

From the server: Start menu → **ButterCams Settings**
(`https://localhost:8650`). From another PC: `https://<server name>:8650`.

The certificate is self-signed, so the browser warns once. Either accept it,
or replace it with one from your own certificate authority under
**System → HTTPS certificate** (PEM certificate + key, or a PFX file).

Mapped drive letters (`Z:\…`) cannot be used anywhere: they do not exist for
services. Use the `\\server\share` path.

## 3. Add destinations

**Destinations → Add destination.**

- **Type:** Local drive (fixed or attached disk) or Network share.
- **Path:** e.g. `E:\Recordings` or `\\nas01\cameras`.
- **User name / password:** only for shares that need different credentials
  than the service account.
- **Limit (GB):** when ButterCams' files on this destination exceed it, the
  oldest are deleted until usage is under 95% of the limit.
- **Max age (days):** optional; older recordings are deleted.

Press **Test** before saving: it creates the folder, writes and reads back a
1 MB file, deletes it, and shows the free space or the exact Windows error.

ButterCams only ever deletes files it wrote itself. Other files in the same
folders are never touched.

## 4. Add cameras

**Cameras → Add camera.** Paste the camera's full RTSP address
(`rtsp://user:password@192.168.1.50:554/stream1`) and it is split into the
fields, or fill in host, port and path yourself. Tick the destinations the
camera's recordings should go to.

**Test connection** connects once and shows the codec, resolution, frame
rate, keyframe interval, audio, and whether the camera allows a second
connection.

Camera settings that matter:

| Setting on the camera | Recommended |
| --- | --- |
| Video codec | H.264 or H.265. MJPEG and MPEG-4 are not recorded |
| I-frame (keyframe) interval | 1–2 seconds. Files and hourly rollovers start on keyframes |
| Audio | AAC, G.711 or G.726 (G.711/G.726 are stored as AAC). Other audio codecs are skipped and the camera's status says so |
| Simultaneous connections | At least 2, so the hourly feed refresh leaves no gap |

Many cameras at once: **Bulk import** takes CSV text with the columns
`name,host,port,path,username,password,destinations` (destinations separated
by `;`).

Recording starts as soon as a camera is saved. The camera's ID (shown under
its name) names its folders and cannot be changed later.

## 5. What gets written where

```
<staging>\<camera-id>\2026-11-14\front-door_2026-11-14_0900_-0600.mp4
<destination>\<camera-id>\2026-11-14\front-door_2026-11-14_0900_-0600.mp4
```

- One file per camera per hour, named with the local date, hour and UTC
  offset. A file ending in `.part.mp4` is being written right now.
- If recording is interrupted within an hour, the next file gets `_b`, then
  `_c`. The Recordings page marks the gap.
- When a file is closed it is copied to each of the camera's destinations,
  verified, and then removed from staging. A camera with no destination
  keeps its files in staging.
- Every file plays in VLC and Windows Media Player as it is.

**Recordings** lists the files per camera and day, shows where each copy is,
and has download links.

## 6. Reading the dashboard

| Status | Meaning |
| --- | --- |
| Healthy | Every enabled camera is recording and copies are keeping up |
| Warning | Something needs attention but recording goes on: a camera not recording for over 2 minutes, a destination failing for over 2 hours, the index was rebuilt |
| Critical | Recordings are being lost or soon will be: a camera not recording for over 15 minutes, staging below its floor, a recorder restarting over and over, the database not writable |

Camera states:

| State | Meaning | What to do |
| --- | --- | --- |
| Recording | Normal | – |
| Connecting | Connecting, or waiting for the first keyframe | Nothing; if it stays, check the keyframe interval |
| Backoff | The connection failed or the stream stalled; retrying (2 s up to every 60 s) | The reason is shown. Usually the camera is off, rebooting or unreachable |
| Error | A setting is wrong; retried every 60 s | "Authentication failed": fix the user name/password. "Camera sends MJPEG…": change the camera's codec |
| Disabled | Switched off in ButterCams | – |

**Live** shows every camera as a tile in a grid of 1, 4, 9 or 16, with its
recording state (refreshed every 5 seconds). Click a tile to see one camera.
**New view** saves a named group of cameras, for example Entrances; saved
views are kept in the browser you made them in. Live video itself is not in
this version: the tiles show state only.

**Events** keeps every incident for 90 days: stalls, restarts, deletions,
status changes, and who changed what.

## 7. When something goes wrong

ButterCams recovers from these by itself; this is what you will see.

| What happened | What ButterCams does | What you see |
| --- | --- | --- |
| A camera drops out | Closes the file, reconnects with growing pauses | Camera in Backoff; a gap on the Recordings page; a Warning after 2 minutes |
| A destination is offline | Keeps files in staging and retries (30 s, 1, 2, 5, 10 minutes, then every 10) | Destination "retrying" with the error and queue length. When it returns, the backlog is copied |
| Staging is nearly full | Deletes the **oldest staging files, even uncopied ones**, so recording can continue | Critical status; a Critical event listing every deleted file. Free up space or fix the destination that stopped taking files |
| A recorder process crashes | Restarted within 2 seconds; the cut-off file is repaired and kept | A Warning event "worker restarted"; the affected cameras continue in a `_b` file |
| The server loses power | The service starts by itself after boot; cut-off files are repaired | Gaps for the time the server was off; files end at most about 3 seconds before the power loss |
| The clock is changed | Starts new files named for the corrected time | A `clock_jump` event |
| `config.json` is damaged | Loads the newest good backup (ten are kept) | Critical status until you check the settings and press Save once |
| The database is damaged | Sets it aside and rebuilds the index from the recordings | A Warning "index rebuilt" |

Things that do need you:

- **Wrong camera password or codec** (state Error).
- **A destination that stays offline**: staging will eventually fill and the
  oldest uncopied files will be deleted.
- **A camera that allows only one connection**: it works, but each hourly
  refresh leaves a gap of a second or two. Raise the camera's connection
  limit if it has such a setting.

## 8. System page

- **Staging:** folder and floor. The floor is the larger of "minimum free %"
  and "minimum free GB". A new folder is used from each camera's next file;
  existing files stay in the old folder until they are copied.
- **Network:** listen address, port, HTTPS, and allowed subnets (CIDR, one
  per line; empty = any address may connect, login still required). After
  changing the port, the firewall rule must be changed too:
  `netsh advfirewall firewall set rule name="ButterCams Web" new localport=<port>`.
- **Users:** administrators can change everything; viewers can look at
  status and download recordings. Five wrong passwords lock an account for
  15 minutes.
- **Log retention:** 1–365 days (logs are also capped at 2 GB).
- **Configuration file:** Export downloads `config.json` without passwords;
  Import replaces the settings (passwords of existing cameras and
  destinations are kept).
- **Rebuild index:** re-reads staging and every destination and brings the
  list of recordings in line with what is on disk. Recording continues while
  it runs.

## 9. Files and logs

| Path | Content |
| --- | --- |
| `<dataRoot>\config.json` (+ `.1` … `.10`) | Settings and the last ten versions |
| `<dataRoot>\users.json` | Accounts (password hashes) |
| `<dataRoot>\buttercams.db` | Index of recordings and the event log |
| `<dataRoot>\logs\buttercams-YYYY-MM-DD.log` | JSON log lines of all processes |
| `<dataRoot>\certs\` | HTTPS certificate and key |
| `HKLM\SOFTWARE\ButterCams\DataRoot` | Where the data folder is |

Service start, stop and Critical events are also written to the Windows
Event Log (Application, source **ButterCams**). Passwords never appear in
logs; RTSP addresses are shown as `rtsp://user:***@host…`.

**Backup:** `config.json` and `users.json` are all you need. Passwords in
`config.json` are encrypted for this computer (Windows DPAPI): after
restoring onto another machine, enter the camera and share passwords again.
The database can always be rebuilt from the recordings.

## 10. Commands

```
buttercams.exe version
buttercams.exe console [--data-root DIR]        run in the foreground (testing)
buttercams.exe rebuild-index [--data-root DIR]  rebuild the index; stop the service first
buttercams.exe setup init ...                   prepare a data folder and the first administrator
buttercams.exe setup install-service ...        register and start the service
buttercams.exe setup uninstall-service [--remove-data]
buttercams.exe setup test-destination --dest-path PATH [--dest-user U]
```

Forgot every administrator password: stop the service, delete
`<dataRoot>\users.json`, run
`buttercams.exe setup init --admin-user NAME --admin-password PASSWORD`,
start the service. Settings and recordings are untouched.

The service checks itself; a monitoring tool can ask too, from the server
itself, without a login: `https://localhost:8650/api/v1/health`.

## 11. Upgrade and uninstall

- **Upgrade:** run the new installer. Settings, accounts, the certificate,
  the index and all recordings are kept. Files being recorded are closed
  cleanly and recording resumes in `_b` files.
- **Uninstall:** Apps & features → ButterCams. You are asked whether to
  remove the data folder. **Recordings on your destinations are never
  deleted.** If you answer No, a later reinstall picks everything up again.

## 12. Development and testing

```
cd web && npm ci && npm run build && cd ..        # build the settings page
go build ./cmd/buttercams                          # build (also works on Linux/macOS)
go test ./...                                      # unit and integration tests

go run ./test/simulator/cmd/camsim --count 8 --audio pcmu     # simulated cameras on rtsp://127.0.0.1:8554/cam1…
./buttercams setup init --data-root ./data --admin-user admin --admin-password "correct horse"
./buttercams console --data-root ./data            # then open https://localhost:8650
```

`test/chaos/chaos.ps1` breaks a running installation at random and checks
that it recovers; see `test/chaos/README.md`. The installer is built with
`installer\build.ps1` (needs Inno Setup 6); `installer\smoke.ps1` is the
service smoke test CI runs. Publishing a release is in `docs/RELEASING.md`.
