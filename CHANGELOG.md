# Changelog

What changed in each version of ButterCams, newest first. The release
workflow publishes the sections of every version since the last release as
the release notes, so write them for the people who install ButterCams.
`bump` (see docs/RELEASING.md) adds the section for a new version.

## 1.0.0

The first release: the core recorder.

- Records many RTSP cameras (60 and more) continuously into one-hour MP4
  files, H.264 and H.265, with gap-free hourly rollover.
- Camera audio is kept: AAC as it is, G.711 and G.726 converted to AAC.
- Copies finished files to the storage destinations you choose, local
  folders or network shares, verifies each copy, and deletes the oldest
  recordings when a destination is full.
- Runs as a Windows service that starts with the PC, watches itself and
  recovers from camera, storage and process failures on its own.
- Set up and monitored from a web page that other PCs on the network can
  open: cameras, destinations, health, events, recordings, users.

Downloads:

- `ButterCams-1.0.0-setup.exe` is the installer; most people want this one.
  See docs/OPERATIONS.md for installing and upgrading.
- `ButterCams-1.0.0-windows-x64.exe` is the program on its own, for trying
  it without installing (`console` mode) or for scripted setups.

The files are not code-signed, so Windows SmartScreen may ask for
confirmation before running them. Check a download against
`SHA256SUMS.txt`.
