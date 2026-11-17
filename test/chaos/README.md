# Chaos test

`chaos.ps1` breaks a running ButterCams installation at random and checks that
it recovers by itself (spec 14.1 and the "chaos" items of 14.3). Run it on a
**test machine** in an elevated PowerShell, with ButterCams installed and
recording, for example from the camera simulator:

```
go run ./test/simulator/cmd/camsim --count 60 --pad 33000 --audio pcmu --listen 0.0.0.0:8554
```

| Action | What it does | Expected |
| --- | --- | --- |
| `kill-worker` | Kills one recorder worker | Back within 2 s; its cameras continue in a `_b` file; under 3 s lost |
| `kill-mover` | Kills the copy process | Restarted; queue resumes; recording untouched |
| `kill-guardian` | Kills the service process | Children die with it; service manager restarts it after 5 s; `.part.mp4` files recovered |
| `block-share` | Blocks SMB to the share host | Recording uninterrupted; destination `retrying`; backlog copies when unblocked |
| `fill-staging` | Fills the staging drive | Oldest staging files deleted; Critical raised; recording continues |
| `clock-jump` | Moves the system clock | New files named for the corrected time; no crash |
| `kill-media` | Kills the live view process | Back within 10 s; every file being recorded keeps growing |
| `pin-media` | Keeps every core busy at the live view process's priority (below normal) | Every file being recorded keeps growing; no stalls in the Events page |
| `viewers` | Opens live streams that connect and vanish at random | Same; needs `-User` and `-Password` |

Examples:

```
.\chaos.ps1 -Rounds 50
.\chaos.ps1 -Rounds 10 -Actions block-share -ShareHost nas01 -BlockSeconds 21600   # six hours offline
.\chaos.ps1 -Rounds 5 -Actions fill-staging,clock-jump
.\chaos.ps1 -Rounds 100 -Actions kill-media                                      # spec 16.11
.\chaos.ps1 -Rounds 10 -Actions pin-media,viewers -User admin -Password "…" -PinSeconds 300
```

`pin-media` does not make the live view process itself spin (that would need a
hook inside ButterCams). It runs one busy loop per core at the same
below-normal priority, which is what a pinned live view process looks like to
the recorders.

The script exits with the number of rounds that did not return to `healthy`
in time and writes everything it did to a log file. Compare that log with the
Recordings page afterwards: every gap must match a round.

The same failures are covered in small by the automated tests, which run on
every push (`go test ./...`):

| Scenario | Test |
| --- | --- |
| Worker killed | `internal/guardian` `TestWorkerCrashRecovery` |
| Mover killed, destination offline | `TestDestinationOutageAndMoverCrash` |
| Staging full | `TestStagingFloorEmergency` |
| Stream stalls | `internal/worker` `TestStallRecovery` |
| Power loss / crash-cut file | `TestCrashRecoveryOfPartFile` |
| Clock change | `TestClockJumpStartsNewFileAtKeyframe` |
| Database corrupt / deleted | `TestCorruptDatabaseIsRebuiltFromDisk`, `TestIndexRebuildEquality` |
| Config unreadable | `TestUnreadableConfigFallsBackToBackup` |
| Live view process killed while watching | `internal/guardian` `TestMediaCrashDoesNotTouchRecording` |
| Restream fails inside a worker | `internal/worker` restream chaos test |
