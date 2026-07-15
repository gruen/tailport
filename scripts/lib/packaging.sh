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
source_tarball_sha256() {
  curl -fsSL "$(source_tarball_url "$1")" | sha256sum | cut -d' ' -f1
}
