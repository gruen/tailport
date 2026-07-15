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

**Not verified here — none of it.** This is an Arch Linux host with no
Homebrew and no Linuxbrew prefix (`brew` is not installed), so the formula has
never been executed: not `brew install --build-from-source`, not `brew test`,
not `brew audit`. The formula is written against Homebrew's documented API
(`std_go_args`, `depends_on ... => :build`, `assert_match`) and its build
recipe matches the `go build` line that `build.yml` and the AUR source PKGBUILD
both use and that *is* verified — but that is reasoning, not evidence.

**What is independently verified:** the `url`/`sha256` pair. The v0.1.5 source
tarball was fetched and hashes to
`dc366a0c57823e5aac342b6085d6a8d0957e108157067e7677c71235d2c7484b`, matching
both this formula and the AUR source package.

Run `brew install --build-from-source ./packaging/brew/tailport.rb` plus
`brew test` and `brew audit --strict --new tailport` on a Mac (or any Linuxbrew
host) before trusting the formula in the tap.
