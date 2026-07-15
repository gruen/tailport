# Shared derivations for the packaging publishers. Sourced, never executed.
#
# The AUR source package (packaging/aur/tailport/PKGBUILD) and the Homebrew
# formula (packaging/brew/tailport.rb) pin the SAME source tarball, so they must
# agree on its digest to the byte. Deriving it in one place means they cannot
# drift: two publishers disagreeing about a digest is the kind of bug that stays
# invisible until a stranger's `brew install` or `paru -S` fails on a checksum
# nobody can reproduce.
#
# See kata 3qyp for the standing risk that this URL's bytes are GitHub's to
# regenerate rather than ours to publish.

PACKAGING_REPO_URL="${PACKAGING_REPO_URL:-https://github.com/gruen/tailport}"

# source_tarball_url <version>  -- version is bare semver, no leading v.
source_tarball_url() {
  printf '%s/archive/refs/tags/v%s.tar.gz\n' "$PACKAGING_REPO_URL" "$1"
}

# source_tarball_sha256 <version>
#
# Fetches to a FILE rather than piping into sha256sum. That is not a style
# choice: POSIX sh has no pipefail and `set -e` does not look inside a
# pipeline, so `curl ... | sha256sum` reports cut(1)'s status and a failed
# fetch makes sha256sum hash EOF -- yielding e3b0c442...b855, the digest of
# the empty string. A caller checking `[ -n "$sha" ]` cannot catch that: the
# value is wrong, not empty. The result would be a GREEN job that publishes an
# unreproducible checksum to the AUR and the tap at once.
#
# Returns non-zero on any failure; `set -e` does fire on `var=$(func)`.
source_tarball_sha256() {
  _pkg_tmp=$(mktemp) || return 1
  # --retry-all-errors covers the 4xx/5xx that plain --retry ignores; this is
  # the container's first network call and a tag archive can lag a fresh push.
  if ! curl -fsSL --retry 3 --retry-delay 2 --retry-all-errors \
       -o "$_pkg_tmp" "$(source_tarball_url "$1")"; then
    rm -f "$_pkg_tmp"
    return 1
  fi
  # A truncated body (mid-stream reset) is worse than an empty one: it hashes
  # to something plausible. Size alone can't prove integrity, but an empty file
  # is the one case we can name outright.
  if [ ! -s "$_pkg_tmp" ]; then
    rm -f "$_pkg_tmp"
    return 1
  fi
  sha256sum "$_pkg_tmp" | cut -d' ' -f1
  rm -f "$_pkg_tmp"
}
