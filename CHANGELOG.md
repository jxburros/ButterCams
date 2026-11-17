# Changelog

What changed in each version of ButterCams, newest first. The release
workflow publishes the sections of every version since the last release as
the release notes, so write them for the people who install ButterCams.
`bump` (see docs/RELEASING.md) adds the section for a new version.

## 2.0.1

Fixes from a review of the whole of ButterCams after version 2.0.0.

Recording:

- Recording can no longer stall, and recorder processes are no longer
  restarted for nothing, while the service is busy with its database (for
  example during an index rebuild).
- Deleting a camera together with its recordings and then adding a camera
  with the same name no longer deletes the new camera's recordings.
- Two destinations, or a destination and the staging folder, can no longer
  be the same folder or one inside the other. That could leave the only
  copy of a recording deleted while the index still listed it. Existing
  destinations set up that way keep recording, with a warning on the
  Dashboard.
- A slow disk no longer makes a camera start extra files as if the clock
  had changed.
- Staging that runs low on space no longer deletes more recordings than
  needed.
- A camera that is switched on gets the normal grace period before it is
  reported as not recording.
- A destination whose copies could never be made no longer shows a queue
  that never empties.
- "buttercams rebuild-index" also finds recordings in earlier staging
  folders.
- The Recordings camera list shows a deleted camera only while it still has
  recordings.

Live view and playback:

- Cameras that send large network packets now play in full-quality live
  view; before, their picture could not be shown.
- Opening a high-resolution camera shows a picture at once instead of after
  the next keyframe, and sound stays in step after a camera reconnects.
- Watching the hour being recorded carries on into the next hour instead of
  stopping at the top of the hour, and playback that runs past midnight
  keeps playing.
- With several cameras side by side, playback keeps going when the first
  camera's recordings end, and a camera that cannot be played is no longer
  retried several times a second.
- Fast playback (8× and 16×) of high-bitrate cameras no longer stops with a
  "buffer full" error.
- Live tiles whose stream cannot be shown no longer reconnect every second.
- Full-quality live view no longer freezes for a few seconds at the top of
  the hour, when the camera's connection is refreshed.
- H.265 recordings report their format correctly to the browser, so
  browsers that play H.265 recognise them.
- Single view of a camera has its own address: the browser's or the
  phone's Back button returns to the grid, and reloading the page keeps the
  camera open.
- "Turn sound on" is shown only when the stream being watched has sound.

Accounts:

- A live picture stops within seconds when the viewer signs out, the account
  is deleted, or the camera is taken away from them; so does the page's live
  status feed.
- Changing a user's cameras together with their role or password is applied
  whole or not at all, and a new account is never left with access to more
  cameras than asked for.
- A Viewer limited to some cameras no longer sees other cameras' file names
  in destination errors.

Other:

- Changes to a camera or a destination are recorded on the Events page,
  like additions and deletions.
- Clearer messages when live view is switched off and after changing only
  the staging free-space floor.

## 2.0.0

Live view and timeline playback: you can now watch your cameras in the
browser, from the same page and login as the settings.

- **Live.** The Live page shows your cameras as moving pictures: 1, 4, 9 or
  16 at a time, from each camera's low-resolution sub stream. Click a tile
  for the full-quality picture with sound. Set the "Sub stream path" of each
  camera (Cameras → edit; "Test connection" checks it) so the grid stays
  light.
- **Recordings on a timeline.** The Recordings page is now a timeline: pick
  a camera and a day, click anywhere to play from that moment, and playback
  runs across the hourly files without a break. Speeds from 0.5× to 16×,
  frame by frame, pictures while you drag, up to four cameras side by side
  in step, and the hour that is being recorded right now. The list of files
  is still there on the "Files" tab.
- **Phones.** Live and the timeline work by touch in Chrome on Android and
  Safari on an iPhone (iOS 17.1 or later).
- **Who sees what.** A Viewer account can be limited to some cameras
  (System → Users). Everyone can save their own camera views; administrators
  can share views with everyone. Views saved in a browser by version 1 are
  offered for import.
- **Recording comes first.** Everything that serves video to browsers runs
  in its own process at lower priority. If it is busy, crashes or is
  switched off (System → Live view and playback), recording and copying carry on
  unchanged.
- Nothing extra to install, no new ports: live view and playback use the
  same HTTPS port, so they also work over Tailscale or WireGuard.

Upgrading from 1.0.0: run the new installer over the old one. Settings,
accounts and recordings are kept; `config.json` is updated to the new format
by itself. H.265 recordings made from this version on also play in Safari;
older H.265 files play in Chrome and offer the download in Safari.

Downloads:

- `ButterCams-2.0.0-setup.exe` is the installer; most people want this one.
  See docs/OPERATIONS.md for installing and upgrading.
- `ButterCams-2.0.0-windows-x64.exe` is the program on its own, for trying
  it without installing (`console` mode) or for scripted setups.

The files are not code-signed, so Windows SmartScreen may ask for
confirmation before running them. Check a download against
`SHA256SUMS.txt`.

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
