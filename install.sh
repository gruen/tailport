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
#
# Version-aware upgrades: if a tailport is already installed at $dest, its
# version and the freshly-downloaded target's version are both read via
# `--version` and compared (semver, 0.x convention -- see is_breaking below).
# A same-version re-run is a no-op. A "breaking" transition (major version
# change, or a 0.x minor change, in EITHER direction) is refused unless
# TAILPORT_ALLOW_BREAKING=1 is set in the environment. Any replace is
# preceded by a single rolling backup at $dest.bak.
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

# read_version PATH
# Prints "X.Y.Z" parsed from `PATH --version`'s expected "tailport X.Y.Z"
# output, or prints nothing if PATH is missing, not executable, exits
# nonzero, or the output doesn't match (an old build with no --version, or an
# unversioned "tailport dev" build). Always returns 0 -- callers distinguish
# "unknown" by checking whether the printed value is empty, not by exit
# status, so this never interacts awkwardly with `set -e`.
read_version() {
	rv_bin="$1"
	if [ -x "$rv_bin" ]; then
		rv_out="$("$rv_bin" --version 2>/dev/null || true)"
	else
		rv_out=""
	fi
	printf '%s\n' "$rv_out" | awk '
		NF == 2 && $1 == "tailport" && $2 ~ /^v?[0-9]+\.[0-9]+\.[0-9]+$/ {
			v = $2
			sub(/^v/, "", v)
			print v
		}
	'
}

# is_breaking INSTALLED TARGET
# Exit 0 if the semver transition INSTALLED -> TARGET is breaking, exit 1
# otherwise. Both arguments are plain "X.Y.Z" strings. Breaking (applies in
# either direction) iff the major version differs, or both sides are pre-1.0
# (major 0) and the minor version differs -- the 0.x convention, matching
# internal/selfupdate's semver semantics for `tailport update`.
is_breaking() {
	awk -v inst="$1" -v tgt="$2" '
		BEGIN {
			split(inst, i, ".")
			split(tgt, t, ".")
			if (i[1] != t[1]) exit 0
			if (i[1] == 0 && t[1] == 0 && i[2] != t[2]) exit 0
			exit 1
		}
	'
}

# is_downgrade INSTALLED TARGET
# Exit 0 if TARGET is an older version than INSTALLED, exit 1 otherwise
# (upgrade or equal). Both arguments are plain "X.Y.Z" strings.
is_downgrade() {
	awk -v inst="$1" -v tgt="$2" '
		BEGIN {
			n = split(inst, i, ".")
			split(tgt, t, ".")
			for (k = 1; k <= n; k++) {
				if (t[k] + 0 < i[k] + 0) exit 0
				if (t[k] + 0 > i[k] + 0) exit 1
			}
			exit 1
		}
	'
}

# Only now, after verification, does anything land at $dest. mv is atomic
# within a filesystem and copy+unlink across one, so a running tailport is
# never truncated mid-write. chmod'ing the verified temp binary here (rather
# than right before the mv) is deliberate: it needs to be executable already
# for the target-version read just below.
mkdir -p "$dest_dir"
chmod +x "$tmp_bin"

target_ver="$(read_version "$tmp_bin")"
installed_ver=""
dest_exists=0
if [ -e "$dest" ]; then
	dest_exists=1
	installed_ver="$(read_version "$dest")"
fi

# Same version already installed: nothing to do. No backup, no write.
if [ "$dest_exists" -eq 1 ] && [ -n "$installed_ver" ] && [ -n "$target_ver" ] \
	&& [ "$installed_ver" = "$target_ver" ]; then
	echo "tailport: already up to date (v$target_ver)" >&2
	exit 0
fi

# Decide whether this transition is breaking. An existing install whose
# version can't be determined (missing --version support, or unparseable
# output) can't be gated -- proceed with an informational note instead of
# blocking the install.
breaking=0
if [ "$dest_exists" -eq 1 ] && [ -z "$installed_ver" ]; then
	echo "tailport: installed version at $dest is unknown (no/unparseable --version output); proceeding without a breaking-change check." >&2
elif [ -n "$installed_ver" ] && [ -n "$target_ver" ] && is_breaking "$installed_ver" "$target_ver"; then
	breaking=1
fi

if [ "$breaking" -eq 1 ] && [ "${TAILPORT_ALLOW_BREAKING:-}" != "1" ]; then
	echo "tailport: refusing breaking upgrade v$installed_ver -> v$target_ver" >&2
	echo "tailport: this is a breaking change (major version, or a 0.x minor change) -- review the release notes before upgrading:" >&2
	echo "tailport:   https://github.com/${repo}/releases" >&2
	echo "tailport: re-run with TAILPORT_ALLOW_BREAKING=1 to install anyway." >&2
	exit 1
fi

if [ "$breaking" -eq 1 ]; then
	echo "tailport: TAILPORT_ALLOW_BREAKING=1 set -- proceeding with BREAKING upgrade v$installed_ver -> v$target_ver" >&2
fi

if [ "$dest_exists" -eq 1 ]; then
	cp -p "$dest" "$dest.bak"
	echo "tailport: backed up existing install to $dest.bak (roll back with: mv $dest.bak $dest)" >&2
fi

mv "$tmp_bin" "$dest"

if [ -z "$target_ver" ]; then
	echo "tailport: installed to $dest" >&2
elif [ -z "$installed_ver" ]; then
	if [ "$dest_exists" -eq 1 ]; then
		echo "tailport: installed v$target_ver to $dest (previous version unknown)" >&2
	else
		echo "tailport: installed v$target_ver to $dest" >&2
	fi
elif is_downgrade "$installed_ver" "$target_ver"; then
	echo "tailport: installed v$installed_ver -> v$target_ver (downgrade) to $dest" >&2
else
	echo "tailport: installed v$installed_ver -> v$target_ver to $dest" >&2
fi
