#!/bin/sh
# Bump both AUR packages to a released version and publish them (kata 18cr).
#
# Lives in a script rather than inline in build.yml so it can be dry-run on any
# Arch box against an ALREADY-published version: re-running it for the current
# pkgver must reproduce the committed PKGBUILDs and .SRCINFO byte-for-byte.
# That is the only way to test this without cutting a throwaway release.
#
#   VERSION=0.1.6 scripts/aur-publish.sh                 # bump in place, no push
#   VERSION=0.1.6 AUR_PUSH=1 scripts/aur-publish.sh      # ...and push to the AUR
#   VERSION=0.1.5 CHECK=1 scripts/aur-publish.sh         # bump + assert git-clean
#
# Run from the repo root, checked out AT THE TAG. That matters: tailport-bin's
# LICENSE/README digests describe what raw.githubusercontent.com serves at the
# tag, so hashing a `main` that has moved on since would write digests that no
# user's makepkg can reproduce.
#
# Binary digests are read from DIST (build.yml's artifacts) rather than fetched
# from the release URL -- the bytes are identical and it can't race asset
# publication. The source tarball digest has no such local source and must be
# fetched (see 3qyp: those bytes are GitHub's to regenerate, not ours).
#
# POSIX sh, no arrays -- matches scripts/test-install.sh.
set -eu

VERSION="${VERSION:?set VERSION to the release version, no leading v (e.g. 0.1.6)}"
DIST="${DIST:-dist}"
AUR_PUSH="${AUR_PUSH:-0}"
CHECK="${CHECK:-0}"
AUR_HOST="${AUR_HOST:-ssh://aur@aur.archlinux.org}"
REPO_URL="https://github.com/gruen/tailport"
# The PKGBUILD maintainer line, not the committer's default identity: AUR
# commits are public and there's no reason to publish a second address.
GIT_NAME="${GIT_NAME:-Michael E. Gruen}"
GIT_EMAIL="${GIT_EMAIL:-contact@michaelgruen.com}"

case "$VERSION" in
  v*) echo "aur-publish: VERSION must not carry a leading v (got '$VERSION')" >&2; exit 2 ;;
esac

say() { printf '==> %s\n' "$*"; }
die() { printf 'aur-publish: %s\n' "$*" >&2; exit 1; }

[ -d packaging/aur ] || die "run me from the repo root (no packaging/aur here)"
command -v makepkg >/dev/null || die "makepkg not found -- this needs an Arch environment"

# makepkg refuses to run as root, and a container job IS root by default. The
# workflow drops to a builder user before calling this; fail loudly rather than
# emitting a PKGBUILD with a silently-missing .SRCINFO.
[ "$(id -u)" -ne 0 ] || die "must not run as root (makepkg refuses); su to an unprivileged user first"

say "collecting digests for $VERSION"

# The one digest with no local source: GitHub generates this tarball on demand.
src_sha=$(curl -fsSL "$REPO_URL/archive/refs/tags/v$VERSION.tar.gz" | sha256sum | cut -d' ' -f1)
[ -n "$src_sha" ] || die "could not hash the source tarball for v$VERSION"

# Prebuilt binary digests, straight from build.yml's artifacts.
for f in "$DIST/tailport-linux-amd64.sha256" "$DIST/tailport-linux-arm64.sha256"; do
  [ -f "$f" ] || die "missing $f -- did the release job's artifacts get downloaded?"
done
amd_sha=$(cut -d' ' -f1 < "$DIST/tailport-linux-amd64.sha256")
arm_sha=$(cut -d' ' -f1 < "$DIST/tailport-linux-arm64.sha256")

# Same bytes raw.githubusercontent.com serves at this tag, because we ARE at it.
lic_sha=$(sha256sum LICENSE   | cut -d' ' -f1)
rdm_sha=$(sha256sum README.md | cut -d' ' -f1)

printf '    source  %s\n    amd64   %s\n    arm64   %s\n    LICENSE %s\n    README  %s\n' \
  "$src_sha" "$amd_sha" "$arm_sha" "$lic_sha" "$rdm_sha"

bump_common() {
  # pkgrel resets to 1: a new pkgver is a new package, not a rebuild of the old.
  sed -i "s/^pkgver=.*/pkgver=$VERSION/; s/^pkgrel=.*/pkgrel=1/" "$1"
}

say "rewriting packaging/aur/tailport/PKGBUILD"
bump_common packaging/aur/tailport/PKGBUILD
sed -i "s|^sha256sums=('[0-9a-f]*')|sha256sums=('$src_sha')|" packaging/aur/tailport/PKGBUILD

say "rewriting packaging/aur/tailport-bin/PKGBUILD"
bump_common packaging/aur/tailport-bin/PKGBUILD
# sha256sums= here is a TWO-line array (LICENSE, README), so this can't be a
# line-oriented sed like the source package's. Rewrite the block wholesale.
# `^sha256sums=\(` deliberately does not match `sha256sums_x86_64=(`.
awk -v lic="$lic_sha" -v rdm="$rdm_sha" -v amd="$amd_sha" -v arm="$arm_sha" '
  /^sha256sums=\(/ {
    printf "sha256sums=(%c%s%c\n", 39, lic, 39
    printf "            %c%s%c)\n", 39, rdm, 39
    skip = 1; next
  }
  /^sha256sums_x86_64=\(/  { printf "sha256sums_x86_64=(%c%s%c)\n",  39, amd, 39; next }
  /^sha256sums_aarch64=\(/ { printf "sha256sums_aarch64=(%c%s%c)\n", 39, arm, 39; next }
  skip && /\)[[:space:]]*$/ { skip = 0; next }   # tail of the old block
  skip { next }
  { print }
' packaging/aur/tailport-bin/PKGBUILD > packaging/aur/tailport-bin/PKGBUILD.new
mv packaging/aur/tailport-bin/PKGBUILD.new packaging/aur/tailport-bin/PKGBUILD

for p in tailport tailport-bin; do
  say "regenerating packaging/aur/$p/.SRCINFO"
  ( cd "packaging/aur/$p" && makepkg --printsrcinfo > .SRCINFO )
  # The AUR rejects a .SRCINFO that disagrees with its PKGBUILD; prove it here
  # rather than discovering it at push time.
  ( cd "packaging/aur/$p" && makepkg --printsrcinfo | diff -q - .SRCINFO >/dev/null ) \
    || die "$p/.SRCINFO does not match its PKGBUILD"
  grep -q "pkgver = $VERSION" "packaging/aur/$p/.SRCINFO" \
    || die "$p/.SRCINFO does not report pkgver $VERSION"
done

if [ "$CHECK" = "1" ]; then
  say "CHECK: asserting the rewrite is a no-op for $VERSION"
  if git diff --quiet -- packaging/aur; then
    say "clean -- reproduced the committed files byte-for-byte"
  else
    git --no-pager diff -- packaging/aur
    die "CHECK failed: rewriting $VERSION did not reproduce the committed files"
  fi
fi

if [ "$AUR_PUSH" != "1" ]; then
  say "AUR_PUSH is not 1 -- stopping before publish"
  exit 0
fi

command -v git >/dev/null || die "git not found"
say "publishing to the AUR"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for p in tailport tailport-bin; do
  say "  $p -> $AUR_HOST/$p.git"
  git clone -q "$AUR_HOST/$p.git" "$work/$p"
  cp "packaging/aur/$p/PKGBUILD" "packaging/aur/$p/.SRCINFO" "$work/$p/"
  # The AUR only accepts refs/heads/master. This repo's init.defaultBranch is
  # main, so an empty clone would otherwise commit onto main and be rejected.
  ( cd "$work/$p" \
    && git symbolic-ref HEAD refs/heads/master \
    && git add PKGBUILD .SRCINFO \
    && if git diff --cached --quiet; then
         echo "    no change for $p at $VERSION -- skipping"
       else
         git -c user.name="$GIT_NAME" -c user.email="$GIT_EMAIL" \
             commit -q -m "$p $VERSION" \
           && git push -q origin HEAD:master \
           && echo "    pushed $p $VERSION"
       fi )
done

say "done"
