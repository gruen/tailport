#!/bin/bash
# entrypoint.sh -- brings up tailscaled, joins the tailnet, exposes Caddy's
# admin API tailnet-only via `tailscale serve`, then runs Caddy itself.
#
# See ../../docs/caddy-edge.md for the full deploy runbook and ./README.md
# for how these env vars map to tailport's `caddy.*` config fields.
set -euo pipefail

DATA_DIR="${DATA_DIR:-/data}"
# Must match tailport's caddy.hostname (default: caddy). Override via Fly's
# [env] in fly.toml if you changed caddy.hostname away from the default.
TS_HOSTNAME="${TS_HOSTNAME:-caddy}"
# Must match tailport's caddy.admin_port (default: 2019) AND
# bootstrap-caddy.json's admin.listen/admin.origins ports.
CADDY_ADMIN_PORT="${CADDY_ADMIN_PORT:-2019}"
TS_SOCKET=/var/run/tailscale/tailscaled.sock

: "${TS_AUTHKEY:?TS_AUTHKEY is required. Set it with 'fly secrets set TS_AUTHKEY=tskey-auth-...' -- never in this file, the Dockerfile, or fly.toml. See docs/caddy-edge.md step 1 for how to mint a tagged, reusable, non-ephemeral key.}"

mkdir -p "$DATA_DIR/tailscale" "$DATA_DIR/caddy-config" "$DATA_DIR/caddy-data"

# caddy run --resume autosaves Caddy's live config under $XDG_CONFIG_HOME
# (falling back to $XDG_CONFIG_HOME/caddy/autosave.json on first boot only,
# when nothing has been autosaved yet -- see caddy run --help). tailport's
# admin-API edits land in that same live config, so pointing both
# XDG_CONFIG_HOME and XDG_DATA_HOME (ACME account/cert storage) at the
# volume is what makes published routes and issued certs survive a restart.
export XDG_CONFIG_HOME="$DATA_DIR/caddy-config"
export XDG_DATA_HOME="$DATA_DIR/caddy-data"

echo "entrypoint: starting tailscaled ..."
tailscaled \
  --state="$DATA_DIR/tailscale/tailscaled.state" \
  --socket="$TS_SOCKET" \
  --port=41641 &
TAILSCALED_PID=$!
trap 'kill "$TAILSCALED_PID" 2>/dev/null || true' EXIT

# Wait for the control socket instead of a guessed sleep -- tailscaled's
# startup time isn't fixed (state size, network conditions).
for _ in $(seq 1 30); do
  [ -S "$TS_SOCKET" ] && break
  sleep 1
done
if [ ! -S "$TS_SOCKET" ]; then
  echo "entrypoint: tailscaled did not create $TS_SOCKET within 30s" >&2
  exit 1
fi

echo "entrypoint: tailscale up (hostname=$TS_HOSTNAME) ..."
# --accept-dns=true (tailscaled's own default) is what puts
# "search <tailnet>.ts.net" in /etc/resolv.conf -- the whole short-MagicDNS-
# label backend-resolution story this edge depends on rests on that line.
# Stated explicitly here rather than left implicit, since docs/caddy-edge.md's
# "route 502s" troubleshooting entry points straight back to it.
tailscale --socket="$TS_SOCKET" up \
  --authkey="$TS_AUTHKEY" \
  --hostname="$TS_HOSTNAME" \
  --accept-dns=true

# Expose Caddy's admin API tailnet-only. This does NOT touch Fly's public
# proxy -- fly.toml.example deliberately has no [[services]] block for this
# port. It's a Tailscale Serve mapping, reachable only from tailnet peers,
# which is what lets tailport (running on some other tailnet machine) drive
# this Caddy's admin API without it ever facing the public internet.
echo "entrypoint: tailscale serve --http=$CADDY_ADMIN_PORT (admin API, tailnet-only) ..."
tailscale --socket="$TS_SOCKET" serve --bg --http="$CADDY_ADMIN_PORT" "$CADDY_ADMIN_PORT"

echo "entrypoint: exec caddy run --resume ..."
exec caddy run --config /etc/caddy/bootstrap-caddy.json --resume
