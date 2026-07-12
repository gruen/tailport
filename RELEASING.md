# Releasing tailport

Maintainer runbook for cutting a tailport release. Short and skimmable —
cross-reference the linked files for the authoritative behavior.

## 1. Prereqs

- `main` is green:
  ```sh
  go build ./... && go vet ./... && go test ./...
  ```
- All release-scoped kata issues are closed with verification evidence.

## 2. Pick the version (semver, pre-1.0 convention)

tailport is pre-1.0, so `install.sh` treats version bumps specially
(`is_breaking` in `install.sh`):

- **PATCH** (`0.1.x` → `0.1.y`): not breaking. `install.sh` upgrades
  existing installs automatically.
- **MINOR** (`0.x` → `0.y`) while still pre-1.0, or any **MAJOR** bump: is
  treated as **BREAKING**. `install.sh` refuses the upgrade unless the user
  sets `TAILPORT_ALLOW_BREAKING=1`:
  ```
  tailport: refusing breaking upgrade v$installed_ver -> v$target_ver
  ...
  tailport: re-run with TAILPORT_ALLOW_BREAKING=1 to install anyway.
  ```

Pick the bump accordingly — a plain fix/feature release should stay a PATCH
so existing fleet installs upgrade without intervention. See README.md
around lines 90-104 for the user-facing description of the same behavior.

## 3. Tag and push

```sh
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

Pushing a `v*` tag triggers `.github/workflows/build.yml`, which:

- Builds the three release matrix targets: **linux/amd64**, **linux/arm64**,
  **darwin/arm64**.
- Injects the version via ldflags (`-X main.version=${GITHUB_REF_NAME#v}`) —
  no source edit needed; `cmd/tailport/main.go` just declares
  `var version = "dev"` as the default.
- Emits a per-binary `.sha256` file (`sha256sum` next to each binary).
- Publishes a GitHub Release via `softprops/action-gh-release@v3` with all
  `dist/*` assets attached.

**This must land as a full release, not a draft or pre-release.**
`install.sh`'s default (unversioned) path downloads from
`.../releases/latest/download/<asset>` — that alias only follows the
latest *published* release, so a draft or pre-release is invisible to it.

## 4. Release notes

`generate_release_notes: true` (this repo, see `build.yml`'s `release` job)
makes GitHub auto-generate the body from commits/PRs since the previous tag.
Nothing to write by hand — after the release publishes, review the
generated notes on the GitHub Release page and augment if anything needs
more context.

## 5. Packaging (manual, separate from build.yml)

Not automated — do this after the release is published:

- **AUR** (`packaging/aur/tailport` and `packaging/aur/tailport-bin`): bump
  `pkgver` (and reset `pkgrel=1`) and update `sha256sums` in both
  `PKGBUILD`s.
  - `tailport` (source): sha256 of the source tarball
    (`https://github.com/gruen/tailport/archive/refs/tags/vX.Y.Z.tar.gz`).
  - `tailport-bin` (prebuilt): sha256 of the LICENSE/README at that tag,
    plus `sha256sums_x86_64` / `sha256sums_aarch64` — these come straight
    from the `.sha256` files build.yml published alongside
    `tailport-linux-amd64` / `tailport-linux-arm64`.
  - See kata jtpx for the AUR package setup, hpx2 for the umbrella.
- **Homebrew**: update the tap formula (`gruen/homebrew-tap`) — see kata
  s3wn.

## 6. install.sh

No per-version edit required. It always resolves `latest` (or an explicit
`TAILPORT_VERSION` pin) and verifies the downloaded binary's sha256 against
the published `.sha256` before installing — see `install.sh` for the
detailed logic.

## 7. Platform coverage note

The build matrix in `build.yml` is the source of truth for what gets
published. Combos it doesn't cover — e.g. `darwin/amd64` (Intel Macs) —
have no release asset; those users fall back to `go install
github.com/gruen/tailport/cmd/tailport@latest` (README's Install section
already documents this).

## 8. Verify (post-publish)

- Run `install.sh` against the real release on a fleet target:
  ```sh
  curl -fsSL https://raw.githubusercontent.com/gruen/tailport/main/install.sh | sh
  ```
  Confirm it fetches the new version, the sha256 verifies, and
  `tailport --version` matches `vX.Y.Z`.
- Confirm the self-update path (`internal/selfupdate`) also sees the new
  release.
