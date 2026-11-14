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

Examples:

```
.\chaos.ps1 -Rounds 50
.\chaos.ps1 -Rounds 10 -Actions block-share -ShareHost nas01 -BlockSeconds 21600   # six hours offline
.\chaos.ps1 -Rounds 5 -Actions fill-staging,clock-jump
```

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
