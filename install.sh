#!/usr/bin/env bash
# Fetches the tailport binary matching this machine's OS/arch from the
# latest GitHub release and installs it to ~/.local/bin/tailport.
set -euo pipefail

repo="gruen/tailport"
dest="${TAILPORT_INSTALL_DIR:-$HOME/.local/bin}/tailport"

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
url="https://github.com/${repo}/releases/latest/download/${asset}"

mkdir -p "$(dirname "$dest")"
echo "tailport: downloading ${asset} from latest release..." >&2
curl -fsSL "$url" -o "$dest"
chmod +x "$dest"
echo "tailport: installed to $dest" >&2
