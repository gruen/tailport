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
is set, so GitHub auto-generates the body from commits/PRs since the previous
tag. **Expect that to give you nothing, and write the notes by hand.** It
summarizes *pull requests*, and work here lands straight on `main` — with no
PRs to draw on it emits a bare "Full Changelog" link and stops. Every release
so far has had hand-written notes attached after the fact:

```sh
gh release edit vX.Y.Z --notes-file <path> --draft=false --prerelease=false
```

The v0.1.4 and v0.1.5 release pages are the house style: a one-sentence
characterization, `## Highlights` with bold lead-ins and bare kata refs in
parens, then `## Install`. The `/release` skill
(`.claude/skills/release/SKILL.md`) drives the whole process and carries the
style guide. Lesson recorded in kata yqb0 and j68f.

## 5. Packaging

- **AUR: automated.** `build.yml`'s `aur` job, gated on `needs: release`,
  runs `scripts/aur-publish.sh` on a tag — it bumps `pkgver`, recomputes all
  six digests, regenerates both `.SRCINFO`, pushes both AUR repos, and
  bot-commits the same bump back to `main`. **So `main` gains a
  `chore(aur): bump PKGBUILDs …` commit after every release — fetch before
  you branch off it.** Dry-run it on any Arch box with
  `VERSION=X.Y.Z CHECK=1 scripts/aur-publish.sh`; the manual recipe in
  `packaging/aur/README.md` is now the fallback path. Setup and rationale:
  kata 18cr, jtpx for the package setup, hpx2 for the umbrella.
- **Homebrew: still manual.** Update the tap formula (`gruen/homebrew-tap`)
  after the release publishes — see kata s3wn.

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
