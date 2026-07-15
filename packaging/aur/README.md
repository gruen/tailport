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
publishing is copying `PKGBUILD` + `.SRCINFO` into each and pushing (see
"Publish to the AUR" below).

## Supporting machinery (already in place)

- `tailport --version` — injected at build via `-ldflags "-X main.version=…"`,
  which is what both PKGBUILDs do. (A plain `go build` reports `dev`; a
  `go install` recovers the version from the embedded module info. Neither
  path applies to packaging — the PKGBUILDs always stamp explicitly.)
- `.github/workflows/build.yml` builds **linux/amd64, linux/arm64, darwin/arm64**
  on a `v*` tag, injects the version, writes a `.sha256` next to each binary,
  and attaches everything to the GitHub release. (linux/arm64 exists purely to
  feed `tailport-bin`'s aarch64 — a release artifact, not a fleet node.)
- Both PKGBUILDs + `.SRCINFO` (generated with `makepkg --printsrcinfo`), with
  real digests. **Currently at 0.1.5.**

## Bumping for a new release

Do this *after* the GitHub release exists (see [RELEASING.md](../../RELEASING.md)) —
every digest below is computed from published artifacts.

### 1. Bump `pkgver` (and reset `pkgrel=1`) in both PKGBUILDs

### 2. Replace every digest — no placeholders

**`tailport/PKGBUILD`** — one digest, the release source tarball:

```sh
curl -sL https://github.com/gruen/tailport/archive/refs/tags/vX.Y.Z.tar.gz | sha256sum
```

**`tailport-bin/PKGBUILD`** — four digests. The two binary digests are already
published by `build.yml`; prefer them over recomputing, then cross-check:

```sh
# authoritative, published next to each asset:
curl -sL https://github.com/gruen/tailport/releases/download/vX.Y.Z/tailport-linux-amd64.sha256
curl -sL https://github.com/gruen/tailport/releases/download/vX.Y.Z/tailport-linux-arm64.sha256
# LICENSE + README, hashed from the raw files at the tag:
curl -sL https://raw.githubusercontent.com/gruen/tailport/vX.Y.Z/LICENSE   | sha256sum
curl -sL https://raw.githubusercontent.com/gruen/tailport/vX.Y.Z/README.md | sha256sum
```

`sha256sums_x86_64` ← amd64, `sha256sums_aarch64` ← arm64. Note the arch names
differ between Arch (`x86_64`/`aarch64`) and Go (`amd64`/`arm64`) — the
`source_*` URLs use the Go names, the `sha256sums_*` arrays use the Arch ones.

### 3. Regenerate both `.SRCINFO` (the AUR requires it to match)

```sh
cd packaging/aur/tailport     && makepkg --printsrcinfo > .SRCINFO
cd packaging/aur/tailport-bin && makepkg --printsrcinfo > .SRCINFO
```

### 4. Verify locally (x86_64 Arch)

Both packages build here — `makepkg` re-downloads every source and validates
the digests you just wrote, so a bad hash fails loudly:

```sh
cd packaging/aur/tailport     && makepkg -f    # runs go test ./... as check()
cd packaging/aur/tailport-bin && makepkg -f
# then confirm the packaged binary reports the new version:
tar xf tailport-X.Y.Z-1-x86_64.pkg.tar.zst -O usr/bin/tailport > /tmp/tp && chmod +x /tmp/tp && /tmp/tp --version
```

`makepkg` refuses to run as root — use a normal user. Clean up `pkg/`, `src/`,
and `*.pkg.tar.*` afterwards; they're build byproducts, not source.

Lint both with `namcap PKGBUILD` and `namcap <built-pkg>.tar.zst`
(`pacman -S namcap`). Two warnings on the built packages are **expected and
correct** — don't "fix" them:

- `Dependency included, but may not be needed ('tailscale'/'iproute2')` on both
  packages. False positive: namcap looks for linkage, and both are runtime
  shell-outs.
- `lacks PIE` / `lacks FULL RELRO` on `tailport-bin` only. That is upstream's
  own release binary, which `build.yml` builds `CGO_ENABLED=0` (hence static,
  hence non-PIE). Shipping it unmodified is the entire point of a `-bin`
  package; the fix would be a `build.yml` change that invalidates every
  published digest. The source package *is* PIE, via `-buildmode=pie`.

A clean-chroot build (`extra-x86_64-build`, from `devtools`) is the stronger
check — it proves `depends`/`makedepends` are complete rather than silently
satisfied by your host. It needs root, so it can't run unattended.

It ends by printing an error that is **expected — ignore it**:

```
error: target not found: tailport
==> WARNING: Skipped checkpkg due to missing repo packages
```

`checkpkg` diffs a freshly built package against the same package *in the
official repos* to catch soname breaks. tailport is an AUR package and has
never been in the official repos, so there is nothing to diff and it skips.
This says nothing about the build, which has already finished by that point —
confirm success by the package appearing in the build dir, not by checkpkg.

## Steps only you (the maintainer) can do

### AUR account (one-time)

Create an account at <https://aur.archlinux.org> and add your SSH public key
under **My Account**. Required to push to `ssh://aur@aur.archlinux.org/<pkg>.git`.

### Publish to the AUR

For each package (`tailport`, then `tailport-bin`):

```sh
git clone ssh://aur@aur.archlinux.org/<pkg>.git aur-<pkg>
cp packaging/aur/<pkg>/PKGBUILD packaging/aur/<pkg>/.SRCINFO aur-<pkg>/
cd aur-<pkg>
git add PKGBUILD .SRCINFO
git commit -m "<pkg> X.Y.Z"
git push
```

## What is / isn't verifiable in this repo's environment

**Verified for 0.1.5 on this x86_64 Arch host:** both packages built with
`makepkg -f` (every source re-downloaded and digest-validated by makepkg
itself), `go test ./...` green as the source package's `check()` step, both
built packages extracted and their `usr/bin/tailport --version` printing
`tailport 0.1.5`, and `makepkg --printsrcinfo` matching each committed
`.SRCINFO` byte-for-byte. `namcap` is clean on both PKGBUILDs (no output) and
reports only the expected warnings above on the built packages. The source
binary was confirmed `ELF … pie executable, dynamically linked` against
`libc.so.6` — which is what makes `glibc` a real, direct dependency rather
than one borrowed from `tailscale`.

`tailport-bin`'s packaged binary was confirmed **byte-for-byte identical** to
the published `tailport-linux-amd64` asset (`84565110…`). That identity is
what `options=('!strip' '!debug')` buys: without it makepkg re-strips the
prebuilt binary *after* makepkg has validated its digest, so the package ships
bytes that no published checksum attests to. Re-check this whenever the -bin
recipe changes — it's the property the whole package exists to provide.

**Also verified for 0.1.5: the source package builds in a clean chroot**
(`extra-x86_64-build`), which is what proves `depends`/`makedepends` are
actually complete rather than quietly satisfied by a dev box that already has
the toolchain. Evidence it was genuinely clean: `.BUILDINFO` records
`builddir = /build` and a from-scratch 165-package environment. The
chroot-built binary prints `tailport 0.1.5` and is PIE + dynamically linked,
and `namcap` on it shows only the two false positives above — no `glibc`
warning, confirming that dependency is now declared correctly.

**Not verified here:** the aarch64 build (x86_64 host — the *digest* is
checked against the published `.sha256`, but the ARM binary is never
executed), and the AUR push itself (needs your account + SSH key).
