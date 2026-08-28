# Caddy edge image for tailport's `P` (publish) path (kata v1z5).
#
# Two upstream images, copied together -- nothing here is rebuilt from
# source. See ./README.md for how this maps to tailport's `caddy.*` config
# fields and ../../docs/caddy-edge.md for the full deploy runbook.
#
# Stage 1: pull the tailscale/tailscaled binaries from the official
# Tailscale image. Fly Machines are real Firecracker microVMs with a real
# TUN device, so stock tailscaled runs here unmodified -- no
# userspace-networking mode, no SOCKS5 shim (verified during v1z5 planning).
FROM docker.io/tailscale/tailscale:stable AS tailscale

# Stage 2: stock Caddy on Alpine, with the tailscale binaries and this
# directory's config layered on top.
FROM docker.io/library/caddy:2-alpine

# bash: entrypoint.sh uses `set -euo pipefail`, which busybox ash does not
# reliably support across Alpine versions -- install real bash rather than
# depend on that.
# ca-certificates: needed for tailscaled's and Caddy's own outbound TLS
# (control-plane / ACME), not for anything this image terminates itself.
RUN apk add --no-cache bash ca-certificates

COPY --from=tailscale /usr/local/bin/tailscaled /usr/local/bin/tailscaled
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscale

RUN mkdir -p /var/run/tailscale

# bootstrap-caddy.json.example is the tracked TEMPLATE -- admin.origins carries
# a placeholder token instead of a real tailnet/hostname, so there is no
# pre-build fill-in step and nothing dev-specific ever lands in the image at
# build time. entrypoint.sh sed-templates the live config from
# $TS_HOSTNAME/$CADDY_ADMIN_PORT at container boot and execs `caddy run`
# against that generated file, not this one. (A gitignored, hand-filled
# bootstrap-caddy.json may still exist in older working trees -- it is
# vestigial and no longer COPYied or used; see ../../docs/caddy-edge.md.)
COPY bootstrap-caddy.json.example /etc/caddy/bootstrap-caddy.json.example
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Everything that must survive a restart -- tailnet node state, Caddy's
# autosaved live config (which is where tailport's published routes and
# issued TLS certs actually live), and Caddy's ACME/OCSP cache -- goes on the
# volume mounted here (see fly.toml.example's [[mounts]]). Nothing under
# /data is baked into the image.
VOLUME ["/data"]

ENTRYPOINT ["/entrypoint.sh"]
