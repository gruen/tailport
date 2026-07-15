# Homebrew packaging for tailport

One formula ships tailport on macOS and Linuxbrew:

| Formula      | What it is                                   | Path                           |
| ------------ | -------------------------------------------- | ------------------------------ |
| `tailport`   | Source build (`go build` from a release tag) | [`tailport.rb`](./tailport.rb) |

Users install it with `brew install gruen/tap/tailport`.

`tailport.rb` is kept here under version control, mirroring
[`packaging/aur/`](../aur/). The real tap is a separate repo —
[github.com/gruen/homebrew-tap](https://github.com/gruen/homebrew-tap) — and
publishing is copying this file to `Formula/tailport.rb` there and pushing (see
"Steps only you (the maintainer) can do").

## The two decisions, and why (s3wn)

- **A personal tap, not homebrew-core.** homebrew-core carries notability and
  ongoing-maintenance bars that buy nothing at this stage. A tap is publishable
  today and the formula is portable to core later without change.
- **A source build, not a prebuilt-binary formula.** `build.yml` produces
  `darwin/arm64` but **no `darwin/amd64`**, so a prebuilt formula would leave
  Intel Macs unserved until the CI matrix grew a target. Building from source
  sidesteps that: Homebrew compiles natively on whatever the user has.
  Revisit only if `brew install` build times become a real complaint — the fix
  is adding `darwin/amd64` to `build.yml` first, not working around it here.

## Why the formula stamps the version explicitly

`install` passes `-X main.version=#{version}`. This is load-bearing, not
cosmetic: the formula builds from a **release tarball**, which carries no VCS
metadata, so the module-info fallback in `cmd/tailport/main.go` resolves to
`(devel)` and the binary would report `dev`. The `test` block asserts the real
version precisely so a dropped stamp fails the formula instead of shipping a
binary that lies about what it is.

`std_go_args` already supplies `-trimpath` and `-o bin/"tailport"`; the `-s -w`
in `ldflags` matches what `build.yml` does for release binaries.

## Bumping for a new release

**Normally you don't** — CI does it. `.github/workflows/build.yml`'s `brew` job
runs after the release publishes and does everything below automatically:
rewrite `url` + `sha256`, push `Formula/tailport.rb` to the tap, and commit the
same bump back to `main`. The logic is
[`scripts/brew-publish.sh`](../../scripts/brew-publish.sh) (kata nqmn), sibling
to the AUR publisher.

It authenticates with a **deploy key scoped to `gruen/homebrew-tap`**, stored as
the `HOMEBREW_TAP_DEPLOY_KEY` secret. It cannot use `GITHUB_TOKEN`: that token
is scoped to the repo whose workflow is running and has no write access to the
tap. The key is disposable — to rotate, generate a new pair, replace the deploy
key on the tap, and reset the secret.

The rest of this section is the **fallback**: for when CI is broken, or to check
its work. To dry-run the script against an already-published version — it must
reproduce the committed formula exactly:

```sh
VERSION=0.1.5 CHECK=1 sh scripts/brew-publish.sh
```

Do this *after* the GitHub release exists (see [RELEASING.md](../../RELEASING.md)) —
the digest is computed from a published artifact.

### 1. Point `url` at the new tag and replace `sha256`

```sh
curl -sL https://github.com/gruen/tailport/archive/refs/tags/vX.Y.Z.tar.gz | sha256sum
```

This is the **same source tarball, and therefore the same digest**, as
`packaging/aur/tailport/PKGBUILD`'s `sha256sums`. If those two disagree, one of
them is wrong — that is a useful cross-check, so bump both together.

### 2. Copy to the tap and push (see below)

`version` is inferred by Homebrew from the `url`, so nothing else needs editing;
the `-X main.version` stamp and the `test` assertion both follow from it.

## Steps only you (the maintainer) can do

### Create the tap (one-time)

The tap repo does not exist yet. It must be a **public** repo named exactly
`homebrew-tap` under your account — Homebrew maps `gruen/tap` to
`github.com/gruen/homebrew-tap` by convention:

```sh
gh repo create gruen/homebrew-tap --public \
  --description "Homebrew tap for tailport"
```

### Publish the formula

```sh
git clone https://github.com/gruen/homebrew-tap.git
mkdir -p homebrew-tap/Formula
cp packaging/brew/tailport.rb homebrew-tap/Formula/tailport.rb
cd homebrew-tap
git add Formula/tailport.rb
git commit -m "tailport X.Y.Z"
git push
```

Then confirm the round trip on a machine with Homebrew:

```sh
brew tap gruen/tap
brew install gruen/tap/tailport
brew test tailport
brew audit --strict --online gruen/tap/tailport
```

## What is / isn't verifiable in this repo's environment

**Nothing here — but that is no longer the end of the story.** This is an Arch
Linux host with no Homebrew, no Linuxbrew prefix, and no ruby, so the formula
cannot be so much as syntax-checked locally. It is not verifiable *on this box*
and never will be.

**It is verified on a macOS runner.**
[`.github/workflows/brew-test.yml`](../../.github/workflows/brew-test.yml) is
where this formula gets proven, and it is the only place that can happen. It is
opt-in — macOS minutes are pricey — so ask for it explicitly:

```sh
gh workflow run brew-test.yml --ref main    # or put [ci brew] in a commit message
```

**Verified for 0.1.6** on `macos-14` (Apple Silicon), run
[29440498745](https://github.com/gruen/tailport/actions/runs/29440498745), kata
8jdh — the first time the formula had ever executed: it taps and parses,
`brew install --build-from-source` compiles it, `brew test` passes (so the
`-X main.version` stamp survived — the binary reports the real version, not
`dev`), `brew audit --strict` is clean, and the installed
`/opt/homebrew/bin/tailport` runs from `PATH` reporting `tailport 0.1.6`.

**Read that run's scope precisely.** The job stages *this repo's copy* of the
formula into a scaffold tap, and the formula pins a release tarball. So it
proves the formula as committed here, building the published v0.1.6 source. It
does **not** prove the tap's published copy, and it says nothing about a
version other than the one `url` currently pins — re-dispatch after a bump.

**The one trap that run exposed, worth knowing before you read a red X.** The
first attempt ([29440235804](https://github.com/gruen/tailport/actions/runs/29440235804))
failed in `brew install` with `go.mod requires go >= 1.26.5 (running go 1.26.4;
GOTOOLCHAIN=local)`. That was the runner, not the formula: GitHub's macOS
images set `HOMEBREW_NO_AUTO_UPDATE=1` and ship a homebrew-core snapshot weeks
stale, so `depends_on "go"` poured a go older than `go.mod` demands, and brew
builds with `GOTOOLCHAIN=local` so go cannot fetch the toolchain itself. Hence
the `brew update` step. A real user's `brew install` auto-updates and gets
current go — but note the sharp edge this leaves: **the formula's source build
requires a Homebrew `go` at least as new as `go.mod`'s pin**, so a user on a
stale brew still fails. `go.mod` currently pins an exact patch (`go 1.26.5`),
which is a tighter constraint than anything in the tree needs — tracked
separately in kata tvyh.

**Also independently verified:** the `url`/`sha256` pair. The v0.1.5 source
tarball was fetched and hashes to
`dc366a0c57823e5aac342b6085d6a8d0957e108157067e7677c71235d2c7484b`, matching
both this formula and the AUR source package.
