# Releasing ButterCams

A release is a GitHub release with three files:

| File | What |
| --- | --- |
| `ButterCams-<version>-setup.exe` | The installer |
| `ButterCams-<version>-windows-x64.exe` | `buttercams.exe` on its own (console mode, scripted setups) |
| `SHA256SUMS.txt` | SHA-256 of the two files, written by the publisher |

Releases are made by the **Release** workflow, started by hand. Merging to
`main` or pushing a tag publishes nothing. Planning, verification and
publication are done by the shared workflows of
[JX-Holdings-LLC/release-infrastructure](https://github.com/JX-Holdings-LLC/release-infrastructure);
its README is the reference for everything below.

## The version

`release.json` holds the version. Two files carry a copy and must agree,
which the **Release versions** check enforces on every pull request:

- `internal/version/version.go` – with a `-dev` suffix, which is what a
  plain `go build` reports. Release and CI builds overwrite it.
- `installer/buttercams.iss` – the default when `AppVersion` is not passed.

**Every change merged to `main` must advance the version** (the toolkit's
`every-main-change` policy), by a patch unless there is a reason for more.
With the toolkit checked out next to the code:

```
git clone https://github.com/JX-Holdings-LLC/release-infrastructure .toolkit
git -C .toolkit checkout <the commit pinned in .github/workflows/release.yml>
node .toolkit/src/release.mjs bump --app buttercams                # patch
node .toolkit/src/release.mjs bump --app buttercams --kind minor
node .toolkit/src/release.mjs bump --app buttercams --version 1.1.0-beta.1
node .toolkit/src/release.mjs validate
```

`bump` updates the three files and adds a `## <version>` section with a
placeholder to `CHANGELOG.md`. Replace the placeholder with what changed,
written for the people who install ButterCams; a release cannot be planned
while a placeholder is left. Not every version is released: the release
notes are the changelog sections of every version since the last published
one. The toolkit needs Node.js 24; `.toolkit/` is ignored by git.

## Making a release

1. Merge to `main` with CI and **Release versions** green.
2. **Actions → Release → Run workflow**, from `main`. Choose **stable**
   (versions like `1.2.3`) or **preview** (versions like `1.3.0-beta.1`,
   published as a pre-release) and leave **dry_run** on.
3. The run builds the installer and the executable on Windows from the exact
   commit that was planned, runs the tests and the service smoke test
   (`installer\smoke.ps1`), and verifies the files. Download the
   `release-buttercams` artifact from the run and try the installer.
4. Run the workflow again with **dry_run** off. It creates the tag
   `v<version>`, uploads the files to a draft, checks what GitHub stored
   against the build, and only then makes the release public.

The run fails, and says why, when there is nothing to release: the version
is already published, or it is a pre-release version on the stable channel
(or the reverse). A published release is never replaced; to fix one, release
a higher version.

## One-time repository settings

The workflows work without these, but they are what makes the rules stick:

- Branch protection on `main` that requires the **Release versions** check
  (and CI). Without it the version rule is only advice.
- The publishing job runs in the GitHub environment `release` (the
  verification job in `release-sign-buttercams`). GitHub creates them on the
  first run without any protection. Add required reviewers to `release` in
  Settings → Environments if a publication should wait for an approval.

## Not done

- The files are **not code-signed**. Windows SmartScreen warns about them
  until they are signed with a certificate; `SHA256SUMS.txt` lets a download
  be checked but does not stop the warning.
- ButterCams does not update itself. An upgrade is running the new
  installer (docs/OPERATIONS.md, section 11).

## Updating the toolkit

`.github/workflows/release.yml` and `release-validate.yml` name one full
commit of release-infrastructure in every `uses:` line, every `toolkit-ref`
and the toolkit checkout. Review the new commit, then change all of them
together; a branch or tag name is refused.
