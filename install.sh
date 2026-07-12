#!/usr/bin/env bash
# shellcheck shell=sh
# Fetches the tailport binary matching this machine's OS/arch from a GitHub
# release, verifies its published sha256 checksum, and installs it to
# ~/.local/bin/tailport (or $TAILPORT_INSTALL_DIR).
#
# Despite the bash shebang (for a direct ./install.sh run), the body below is
# written to POSIX sh so it also works piped into dash-based /bin/sh:
#     curl -fsSL .../install.sh | sh
# The `shellcheck shell=sh` directive above lints it as POSIX, so bashisms
# (which would break that pipe under dash) are caught in CI.
set -eu

repo="gruen/tailport"
dest_dir="${TAILPORT_INSTALL_DIR:-$HOME/.local/bin}"
dest="$dest_dir/tailport"

# Undocumented test seam: overrides the release base URL (everything up to,
# but not including, "/latest/..." or "/download/..."). Defaults to the real
# GitHub releases URL; only the install-script test harness sets this, to
# point downloads at a local fixture tree. Not a supported/public knob.
base_url="${TAILPORT_BASE_URL:-https://github.com/${repo}/releases}"

os="$(uname -s)"
case "$os" in
	Linux) goos="linux" ;;
	Darwin) goos="darwin" ;;
	*)
		echo "tailport: unsupported OS: $os" >&2
		exit 1
		;;
esac

arch="$(uname -m)"
case "$arch" in
	x86_64 | amd64) goarch="amd64" ;;
	arm64 | aarch64) goarch="arm64" ;;
	*)
		echo "tailport: unsupported architecture: $arch" >&2
		exit 1
		;;
esac

asset="tailport-${goos}-${goarch}"

# TAILPORT_VERSION pins a release; accepts "0.1.1" or "v0.1.1". Unset/empty
# means install the latest release.
version="${TAILPORT_VERSION:-}"
if [ -z "$version" ]; then
	url="${base_url}/latest/download/${asset}"
else
	version="${version#v}"
	url="${base_url}/download/v${version}/${asset}"
fi
sha_url="${url}.sha256"

command -v curl >/dev/null 2>&1 || {
	echo "tailport: curl is required to install tailport" >&2
	exit 1
}

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT INT TERM

tmp_bin="$tmpdir/$asset"
tmp_sha="$tmpdir/$asset.sha256"

if [ -z "$version" ]; then
	echo "tailport: downloading ${asset} from the latest release..." >&2
else
	echo "tailport: downloading ${asset} (v${version})..." >&2
fi
curl -fsSL "$url" -o "$tmp_bin"
curl -fsSL "$sha_url" -o "$tmp_sha"

# The .sha256 file is "<hash>  <asset-name>" (sha256sum's GNU format). Read
# just the hash rather than using `sha256sum -c`, since the recorded filename
# won't match our temp path (and -c isn't portable to macOS anyway).
expected="$(awk '{ print $1; exit }' "$tmp_sha")"
if [ -z "$expected" ]; then
	echo "tailport: could not read a checksum from $sha_url" >&2
	exit 1
fi

# Portable hash of the download: Linux has sha256sum, macOS has shasum (not
# sha256sum); fall back to openssl. Missing all three is a hard failure --
# never install an unverified binary.
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp_bin" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp_bin" | awk '{ print $1 }')"
elif command -v openssl >/dev/null 2>&1; then
	actual="$(openssl dgst -sha256 "$tmp_bin" | awk '{ print $NF }')"
else
	echo "tailport: need sha256sum, shasum, or openssl to verify the download" >&2
	exit 1
fi

if [ "$actual" != "$expected" ]; then
	echo "tailport: checksum mismatch for ${asset} -- refusing to install" >&2
	echo "tailport: expected $expected" >&2
	echo "tailport: got      $actual" >&2
	exit 1
fi

# Only now, after verification, does anything land at $dest. mv is atomic
# within a filesystem and copy+unlink across one, so a running tailport is
# never truncated mid-write.
mkdir -p "$dest_dir"
chmod +x "$tmp_bin"
mv "$tmp_bin" "$dest"
echo "tailport: installed to $dest" >&2
