#!/bin/sh
# Bump the Homebrew formula to a released version and publish it to the tap
# (kata nqmn). Sibling of scripts/aur-publish.sh; same shape, same CHECK=1
# self-test, same reason for existing as a script rather than inline YAML --
# so it can be dry-run against an already-published version instead of only
# being testable by cutting a throwaway release.
#
#   VERSION=0.1.6 scripts/brew-publish.sh                # rewrite in place
#   VERSION=0.1.6 BREW_PUSH=1 scripts/brew-publish.sh    # ...and push to the tap
#   VERSION=0.1.5 CHECK=1 scripts/brew-publish.sh        # assert it's a no-op
#
# Unlike the AUR publisher this needs no Arch container and no makepkg: the
# formula is a url + sha256 rewrite. Homebrew infers `version` from the url, so
# the -X main.version stamp and the test block's assertion both follow from it
# automatically -- there is no third place to keep in sync.
#
# Pushing needs a credential: GITHUB_TOKEN is scoped to the repo whose workflow
# is running and cannot write to the tap. CI supplies a deploy key scoped to
# gruen/homebrew-tap alone via GIT_SSH_COMMAND.
#
# POSIX sh, matching scripts/aur-publish.sh and scripts/test-install.sh.
set -eu

VERSION="${VERSION:?set VERSION to the release version, no leading v (e.g. 0.1.6)}"
BREW_PUSH="${BREW_PUSH:-0}"
CHECK="${CHECK:-0}"
TAP_REMOTE="${TAP_REMOTE:-git@github.com:gruen/homebrew-tap.git}"
FORMULA=packaging/brew/tailport.rb
GIT_NAME="${GIT_NAME:-Michael E. Gruen}"
GIT_EMAIL="${GIT_EMAIL:-contact@michaelgruen.com}"

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/lib/packaging.sh"

case "$VERSION" in
  v*) echo "brew-publish: VERSION must not carry a leading v (got '$VERSION')" >&2; exit 2 ;;
esac

say() { printf '==> %s\n' "$*"; }
die() { printf 'brew-publish: %s\n' "$*" >&2; exit 1; }

[ -f "$FORMULA" ] || die "run me from the repo root (no $FORMULA here)"

say "collecting the source tarball digest for $VERSION"
url=$(source_tarball_url "$VERSION")
sha=$(source_tarball_sha256 "$VERSION")
[ -n "$sha" ] || die "could not hash the source tarball for v$VERSION"
printf '    url    %s\n    sha256 %s\n' "$url" "$sha"

say "rewriting $FORMULA"
# Anchored to the two-space indent Homebrew formulae use, so this cannot
# accidentally match a url/sha256 mentioned inside a comment or the test block.
sed -i \
  -e "s|^  url \".*\"|  url \"$url\"|" \
  -e "s|^  sha256 \".*\"|  sha256 \"$sha\"|" \
  "$FORMULA"

grep -q "^  url \"$url\"" "$FORMULA"       || die "url rewrite did not take"
grep -q "^  sha256 \"$sha\"" "$FORMULA"    || die "sha256 rewrite did not take"

# Homebrew parses the version out of the url; if that ever stops lining up, the
# formula would install X.Y.Z while stamping the binary with something else.
case "$url" in
  *"/v$VERSION.tar.gz") : ;;
  *) die "url does not end in v$VERSION.tar.gz -- brew would infer the wrong version" ;;
esac

if [ "$CHECK" = "1" ]; then
  say "CHECK: asserting the rewrite is a no-op for $VERSION"
  if git diff --quiet -- "$FORMULA"; then
    say "clean -- reproduced the committed formula byte-for-byte"
  else
    git --no-pager diff -- "$FORMULA"
    die "CHECK failed: rewriting $VERSION did not reproduce the committed formula"
  fi
fi

if [ "$BREW_PUSH" != "1" ]; then
  say "BREW_PUSH is not 1 -- stopping before publish"
  exit 0
fi

command -v git >/dev/null || die "git not found"
say "publishing to $TAP_REMOTE"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git clone -q "$TAP_REMOTE" "$work/tap"
mkdir -p "$work/tap/Formula"
cp "$FORMULA" "$work/tap/Formula/tailport.rb"
( cd "$work/tap" \
  && git add Formula/tailport.rb \
  && if git diff --cached --quiet; then
       echo "    tap already at $VERSION -- nothing to push"
     else
       git -c user.name="$GIT_NAME" -c user.email="$GIT_EMAIL" \
           commit -q -m "tailport $VERSION" \
         && git push -q origin HEAD \
         && echo "    pushed tailport $VERSION to the tap"
     fi )

say "done"
