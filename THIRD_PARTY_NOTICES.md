# Third-party software in ButterCams

ButterCams itself is released under the MIT license (see `LICENSE`). The
`buttercams.exe` binary is built from these open-source components. Their
full license texts are in the module sources, which `go mod download` and
`npm ci` fetch at the versions pinned in `go.sum` and `web/package-lock.json`.

## Go modules

| Module | License |
| --- | --- |
| github.com/bluenviron/gortsplib/v5 | MIT |
| github.com/bluenviron/mediacommon/v2 | MIT |
| github.com/abema/go-mp4 | MIT |
| github.com/pion/rtp, rtcp, sdp, srtp, transport, logging, randutil | MIT |
| github.com/tphakala/go-aac | **LGPL-2.1-or-later** (see below) |
| github.com/tphakala/simd | MIT |
| modernc.org/sqlite, libc, mathutil, memory | BSD-3-Clause |
| golang.org/x/crypto, golang.org/x/sys, golang.org/x/net | BSD-3-Clause |
| software.sslmate.com/src/go-pkcs12 | BSD-3-Clause |
| github.com/google/uuid | BSD-3-Clause |
| github.com/gorilla/websocket | BSD-2-Clause |
| github.com/dustin/go-humanize, github.com/mattn/go-isatty, github.com/ncruces/go-strftime | MIT |
| github.com/remyoudompheng/bigfft | BSD-3-Clause |
| The Go standard library and runtime | BSD-3-Clause |

## Web UI

| Package | License |
| --- | --- |
| react, react-dom | MIT |
| react-router, react-router-dom | MIT |
| @fontsource/bricolage-grotesque, @fontsource/instrument-sans, @fontsource/martian-mono (font files bundled into the page) | Fonts: SIL Open Font License 1.1; packaging: MIT |

## About go-aac (LGPL)

ButterCams uses `github.com/tphakala/go-aac` to convert G.711 and G.726
camera audio to AAC. That library is a Go port of FFmpeg's AAC encoder and is
licensed under the GNU Lesser General Public License, version 2.1 or later.

The LGPL lets you use the library in a program under another license,
provided the recipient can replace the library with a modified version. With
ButterCams you can: the complete source of ButterCams is available under the
MIT license, so you can change or replace go-aac and rebuild `buttercams.exe`
with `go build ./cmd/buttercams`. The source of go-aac at the exact version
used is at https://github.com/tphakala/go-aac (see `go.mod` for the version).

If ButterCams is ever distributed without its source, the LGPL obligations
have to be met another way; `docs/DECISIONS.md` (decision 1) describes how the
audio converter can be swapped out.
