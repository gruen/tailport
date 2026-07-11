# AUR packaging for tailport

Two AUR packages ship tailport on Arch:

| Package        | What it is                                    | Path                          |
| -------------- | --------------------------------------------- | ----------------------------- |
| `tailport`     | Source build (`go build` from a release tag)  | [`tailport/`](./tailport/)    |
| `tailport-bin` | Prebuilt binary from a GitHub release asset   | [`tailport-bin/`](./tailport-bin/) |

Both target `x86_64` and `aarch64`, are MIT-licensed, and depend on
`tailscale` + `iproute2` (`ss` lives in iproute2; `lsof`/macOS is not an Arch
dep). The source package additionally `makedepends=('go')` (go.mod needs
go ≥ 1.26.5 — make sure Arch's `go` is new enough at package time).

The PKGBUILDs and generated `.SRCINFO` files are kept here under version
control. The real AUR repos are separate git remotes, one per package;
publishing is copying `PKGBUILD` + `.SRCINFO` into each and pushing (step 5
below).

## What's already done (in this repo)

- `tailport --version` (injected at build via `-ldflags "-X main.version=…"`;
  `dev` for plain `go build`).
- `.github/workflows/build.yml` builds **linux/amd64, linux/arm64, darwin/arm64**
  on a `v*` tag, injects the version, writes a `.sha256` next to each binary,
  and attaches everything to the GitHub release. (linux/arm64 was added purely
  to feed `tailport-bin`'s aarch64 — it is a release artifact, not a fleet node.)
- Both PKGBUILDs + `.SRCINFO` (generated with `makepkg --printsrcinfo`).

The four `sha256sums*` entries are placeholders (`REPLACE_WITH_SHA256_…`) —
they can only be filled once the v0.1.0 tag and its release assets exist
(steps 2–3).

## Steps only you (the maintainer) can do

### 1. AUR account (one-time)

Create an account at <https://aur.archlinux.org> and add your SSH public key
under **My Account**. Required to push to `ssh://aur@aur.archlinux.org/<pkg>.git`.

### 2. Cut the v0.1.0 tag + GitHub release

```sh
git tag -a v0.1.0 -m "tailport 0.1.0"
git push origin v0.1.0
```

The tag push triggers `build.yml`, which builds the three targets and creates
the GitHub release with the binaries and their `.sha256` files attached.
Confirm the release has:
`tailport-linux-amd64`, `tailport-linux-arm64`, `tailport-darwin-arm64`
(+ each `.sha256`).

### 3. Fill in the sha256sums

**Source PKGBUILD** (`tailport/PKGBUILD`) — hash the release source tarball:

```sh
cd packaging/aur/tailport
# either:
makepkg -g >> PKGBUILD        # then delete the old placeholder line
# or compute directly:
curl -sL https://github.com/gruen/tailport/archive/refs/tags/v0.1.0.tar.gz | sha256sum
```

**Binary PKGBUILD** (`tailport-bin/PKGBUILD`) — four hashes:

- `sha256sums` (LICENSE, README): from the raw files at the tag, e.g.
  `curl -sL https://raw.githubusercontent.com/gruen/tailport/v0.1.0/LICENSE | sha256sum`
- `sha256sums_x86_64` / `sha256sums_aarch64`: published as
  `tailport-linux-amd64.sha256` / `tailport-linux-arm64.sha256` on the release
  (or `makepkg -g` will fetch and hash them).

Then regenerate each `.SRCINFO`:

```sh
cd packaging/aur/tailport     && makepkg --printsrcinfo > .SRCINFO
cd packaging/aur/tailport-bin && makepkg --printsrcinfo > .SRCINFO
```

Commit the filled hashes + updated `.SRCINFO`.

### 4. Lint & build in a clean chroot (recommended)

Not doable in the dev sandbox here (no `namcap`, no C compiler for the
source package's `-linkmode=external`, no ARM box). On an Arch machine with
`devtools` + `namcap`:

```sh
cd packaging/aur/tailport
namcap PKGBUILD
extra-x86_64-build          # clean-chroot build (base-devel provides gcc → cgo/PIE work)
namcap tailport-*.pkg.tar.zst
# aarch64 needs an ARM host or cross toolchain.
```

For `tailport-bin`, `makepkg -f` after the real assets exist; `namcap` both.

### 5. Publish to the AUR

For each package (`tailport`, then `tailport-bin`):

```sh
git clone ssh://aur@aur.archlinux.org/<pkg>.git aur-<pkg>
cp packaging/aur/<pkg>/PKGBUILD packaging/aur/<pkg>/.SRCINFO aur-<pkg>/
cd aur-<pkg>
git add PKGBUILD .SRCINFO
git commit -m "Initial import: <pkg> 0.1.0"
git push
```

## What was verified in-env vs. left to you

Verified here: `--version` (default + injected), `go test ./...` (the `check()`
step), the `build.yml` recipe for all three targets (linux/arm64 cross-compiles
to a valid aarch64 ELF; version injection confirmed), both `.SRCINFO` generate
cleanly, and the source `package()` install lines produce the correct FHS tree
with a version-reporting binary.

Left to you (can't run in the sandbox): filling real sha256sums (needs the
tag/release), the source PKGBUILD's `-linkmode=external`/`-buildmode=pie` build
(needs a C toolchain — present in an Arch clean chroot via base-devel),
`namcap` lint, the aarch64 build (needs an ARM host), and the AUR account +
push.
