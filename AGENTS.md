# Instructions for coding agents

**Read `BUTTERCAMS_SPEC.md` first.** It is the source of truth for what
ButterCams is and how each part must work. Section 1 (the non-negotiable
rules) overrides convenience: any change that can block, slow or crash the
recording path is a bug.

Then read:

- `docs/DECISIONS.md` – choices already made where the spec left room. Add an
  entry whenever you make another one.
- `docs/IDEAS.md` – where ideas that are not in the spec go (rule 8: do not
  build them).
- `docs/API.md` – the REST contract. Existing endpoints never change.
- `docs/OPERATIONS.md` – the administrator's guide; keep it true.
- `docs/V2_PLAN.md` – the build plan for the next version, live view and
  timeline playback (spec section 16): order of work, code changes, tests.

## Layout

| Path | What |
| --- | --- |
| `cmd/buttercams` | Sub-command dispatch and the `setup` helpers the installer calls |
| `internal/guardian` | The service: supervision, camera assignment, index owner, retention decisions, health |
| `internal/worker` | Recorder: camera state machine, RTSP sessions, fMP4 writer, hourly rollover |
| `internal/mover` | Copy with verification, retention selection, deletes |
| `internal/api` | HTTPS server, accounts, handlers, event stream |
| `internal/config`, `internal/index`, `internal/ipc` | config.json, SQLite index, JSON-lines messages |
| `internal/audio`, `internal/mp4meta`, `internal/naming`, `internal/logx` | G.711/G.726 → AAC, MP4 metadata and repair, file naming, logging |
| `internal/winsvc` | Everything Windows-specific, with stand-ins for other systems |
| `web` | React + Vite settings page, embedded in the binary |
| `test/simulator`, `test/chaos` | Camera simulator and chaos script |
| `installer` | Inno Setup script and build script |

## Working here

```
cd web && npm ci && npm run build && cd ..   # once, and after UI changes
gofmt -l cmd internal test                   # must print nothing
go vet ./... && GOOS=windows go vet ./...    # both must pass
go test ./...                                # unit + integration tests (about two minutes)
```

- The target is Windows, but the code builds and the tests run on Linux and
  macOS too. Keep it that way: Windows-only code lives in
  `internal/winsvc/*_windows.go` (and `internal/config/secret_windows.go`)
  with a matching `_other.go`.
- No CGO, no telemetry, no cloud services, nothing that must be installed
  separately (spec section 3).
- The recorder tests decode every file they produce with ffmpeg when it is
  installed; install it locally so you see playback problems before CI does.
- Workers and the mover never open the database; they talk to the guardian
  over stdin/stdout (`internal/ipc`). Nothing in `internal/api` may wait on a
  worker.
- Retention and cleanup may only touch files that are in the index and whose
  names match `internal/naming`.
- v2 (spec section 16) is the next phase; build it in the order of
  `docs/V2_PLAN.md`. Its rules for protecting the recorder are in 16.9.
- v3 and v4 (spec sections 17–18) are built on the hooks of section 15 and
  on v2. Do not start them without being asked.
