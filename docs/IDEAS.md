# Ideas outside the specification

Rule 8 of the spec: do not add features that are not in it; write ideas here.
Nothing in this file is implemented. Items already planned for v2–v4 in the
spec are not repeated.

## Recording

- **Checksum sidecars.** A rebuild cannot restore a file's SHA-256 (it is
  recomputed at the next copy). A small `<name>.sha256` next to each file
  would make a rebuilt index complete, at the cost of a second file per hour.
- **Bandwidth limit for copies.** The mover is bounded to four concurrent
  copies; a per-destination MB/s limit would help on slow WAN links.
- **Copy window.** Only copy to a destination during given hours (off-peak).
- **RTSPS and ONVIF discovery.** Encrypted RTSP is already supported by the
  RTSP library; the camera form has no field for it. ONVIF discovery could
  fill in the camera list.
- **16 kHz AAC.** The pure-Go AAC encoder only supports 44.1/48 kHz, so
  telephone audio is upsampled to 48 kHz. An encoder that supports 16 kHz
  would match the spec's wording and save a little CPU.
- **Per-camera staging folder**, to spread sixty cameras over several disks.

## Operations

- **MSI package** for deployment through Group Policy (the v1 installer is
  an Inno Setup executable; see `docs/DECISIONS.md`, decision 29).
- **Firewall rule follows the port.** Changing the web port on the System
  page does not update the Windows Firewall rule; it has to be done by hand.
- **Sessions that survive a service restart.** Sessions are in memory.
- **Password reset command** instead of deleting `users.json`.
- **Windows Performance Counters** for bitrate, queue length and free space.
- **Staging on two disks** with automatic fail-over when one fills or fails.
- **Configuration change history** in the UI (the last ten versions of
  `config.json` are already kept on disk).
- **Code signing** of `buttercams.exe` and the installer.

## Web UI

- Sort and filter the camera table; show cameras per worker.
- A day overview per camera (24 bars with gaps) – part of the v2 timeline
  (spec 16.5).
- Dark theme.

## Live view and playback

- **WebRTC transport** for live view (lower latency, and it works on older
  iPhones). Left out of v2 on purpose; see `docs/DECISIONS.md`, decision 36.
- **HLS fallback** for browsers without Media Source Extensions.
- **Reverse playback.** v2 has frame step back and scrubbing only.
- **Snapshot wall for 25+ cameras**: one keyframe per camera every few
  seconds instead of moving video.
