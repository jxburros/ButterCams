# ButterCams

ButterCams, because the cameras will run smooth as butter.

A Windows service that records many RTSP IP cameras (60 and more)
continuously into one-hour MP4 files, copies them to the storage you choose,
deletes the oldest files when a destination is full, watches itself and
recovers from failures on its own. It is set up from a web page that other
PCs on the network can open, and the same page shows the cameras live and
plays the recordings on a timeline.

- **Version 1 – Core Recorder** is implemented here: recording, hourly files
  with gap-free rollover, audio, storage destinations and retention,
  self-recovery, health monitoring, the settings page and API, the installer.
- **Version 2 – Live View and Timeline Playback** is implemented here too
  (section 16 of the spec): live video in the browser from sub streams and
  main streams, a timeline that plays across the hourly files, per-user
  camera limits and saved views, all served by a separate process so that
  watching can never disturb recording. What was verified and what still
  needs the target server is listed at the end of
  [docs/DECISIONS.md](docs/DECISIONS.md).
- Versions 3 and 4 (alerts, clip export and motion; AI detection) are
  specified but not built.

## Documents

| | |
| --- | --- |
| [BUTTERCAMS_SPEC.md](BUTTERCAMS_SPEC.md) | The build specification (source of truth) |
| [docs/V2_PLAN.md](docs/V2_PLAN.md) | How version 2 (live view and timeline playback) was built |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Installing, configuring and troubleshooting |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Choices made during the build, and what has not been verified yet |
| [docs/API.md](docs/API.md) | REST API |
| [docs/RELEASING.md](docs/RELEASING.md) | Versions, the changelog and publishing a release |
| [CHANGELOG.md](CHANGELOG.md) | What changed in each version |
| [docs/IDEAS.md](docs/IDEAS.md) | Ideas outside the specification |
| [AGENTS.md](AGENTS.md) | Notes for coding agents |

## Download

The Windows installer (`ButterCams-<version>-setup.exe`) and the program on
its own (`ButterCams-<version>-windows-x64.exe`) are on the
[releases page](https://github.com/jxburros/ButterCams/releases).
[docs/OPERATIONS.md](docs/OPERATIONS.md) covers installing.

## Build and try it

Needs Go 1.26 or newer and Node.js 22.

```
cd web && npm ci && npm run build && cd ..
go build ./cmd/buttercams
go test ./...
```

Try it without cameras (works on Windows, Linux and macOS):

```
go run ./test/simulator/cmd/camsim --count 8 --audio pcmu --sub
./buttercams setup init --data-root ./data --admin-user admin --admin-password "correct horse"
./buttercams console --data-root ./data
```

Open `https://localhost:8650`, sign in, and add cameras with addresses
`rtsp://127.0.0.1:8554/cam1` … `/cam8` and the sub stream paths `/cam1/sub` …
`/cam8/sub`. The Live page then shows them, and Recordings plays what was
recorded.

The Windows installer and executable are built with `installer\build.ps1`;
CI builds them on every push and attaches the installer to the run. Releases
are published by the Release workflow, see
[docs/RELEASING.md](docs/RELEASING.md).

## License

MIT, see [LICENSE](LICENSE). Third-party components and their licenses are
listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md); note the LGPL
audio encoder described there.
