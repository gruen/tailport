# Caddy edge packaging for tailport

Build artifacts for the **Caddy edge**: a small, user-run Docker image
(tailscaled + Caddy) that lets tailport publish a local port to a custom
public hostname (`https://app.example.com`) over your tailnet — the `P`
(publish) path, kata v1z5. It mirrors [`packaging/aur/`](../aur/) and
[`packaging/brew/`](../brew/): the artifacts live here under version
control; deploying them (a Fly.io app, in this case) is a step you run
yourself, not something CI does on a release tag.

**This is not required to use tailport.** It's opt-in infrastructure for the
one feature (`P`) that needs a public-facing component. Everything else —
`tailscale serve`/`space`, Funnel/`p` — works with no edge at all.

For the full deploy runbook (Tailscale ACL, Fly setup, DNS, first-publish
smoke test, troubleshooting), see
[`docs/caddy-edge.md`](../../docs/caddy-edge.md). This README covers what's
*in* this directory and how it maps to tailport's config.

## What's here

| File | What it is |
| --- | --- |
| [`Dockerfile`](./Dockerfile) | Multistage build: copies `tailscale`/`tailscaled` from the official Tailscale image onto stock `caddy:2-alpine`. Nothing is rebuilt from source. |
| [`entrypoint.sh`](./entrypoint.sh) | Container entrypoint: starts `tailscaled`, joins the tailnet, exposes Caddy's admin API tailnet-only via `tailscale serve`, then `exec`s `caddy run --resume`. |
| [`bootstrap-caddy.json.example`](./bootstrap-caddy.json.example) | Template for Caddy's first-boot admin config: an empty `tailport` HTTP server on `:443`/`:80`, plus a widened `admin.origins` list (see below). Copy it to `bootstrap-caddy.json` (gitignored, like `fly.toml`) and fill in your tailnet before building — that copy is what the image bakes in. |
| [`fly.toml.example`](./fly.toml.example) | Template for a Fly.io deployment: raw-TCP passthrough on 80/443, the state volume, and deliberately no service for the admin port. |

## How this maps to tailport's `caddy.*` config

tailport's own config (`~/.config/tailport/config.yaml`, documented in the
root [README](../../README.md#configuration)) has a `caddy:` block with four
fields that must agree with what this edge is actually running as:

| tailport config field | Default | What it must match here |
| --- | --- | --- |
| `caddy.hostname` | `caddy` | The `--hostname` `tailscale up` registers (`TS_HOSTNAME` in `entrypoint.sh`, default `caddy`). This is the edge's own tailnet identity — how tailport *finds* the admin API — not any published route's public hostname. |
| `caddy.admin_port` | `2019` | `bootstrap-caddy.json`'s `admin.listen`/`admin.origins` ports, `entrypoint.sh`'s `CADDY_ADMIN_PORT`, and the `tailscale serve --http=<port> <port>` mapping it sets up. |
| `caddy.server_name` | `tailport` | `bootstrap-caddy.json`'s `apps.http.servers.<server_name>` key — the one shared Caddy HTTP server every tailport computer publishing through this edge writes routes into. |
| `caddy.domain` | `""` (blank) | Not part of this image at all — it's the public base domain tailport builds *publish* hostnames from (`<prefix>.<domain>`), pointed at this edge's DNS (see the runbook's DNS step). Unrelated to the edge's own tailnet hostname above. |

If you change any of `hostname`/`admin_port`/`server_name` away from their
defaults in tailport's config, you must change the matching value here too
(`bootstrap-caddy.json` and/or `fly.toml`'s `[env]`) — they are not read from
tailport's config file; this image and tailport's config are two independent
places that must be kept in sync by hand.

## Why `bootstrap-caddy.json`'s `admin.origins` is widened

Caddy's admin API checks the incoming request's `Host` header against an
allow-list (`admin.origins`) and returns `403` for anything else — a
DNS-rebinding mitigation. By default that list is just `localhost:2019` /
`127.0.0.1:2019`. But tailport reaches this admin API over the tailnet at
`http://<caddy.hostname>:<caddy.admin_port>` (e.g. `http://caddy:2019`), so
the `Host` header on that request is `caddy:2019`, not `localhost:2019` —
which would 403 against the unmodified default.

The template widens `admin.origins` to include that address (and the full
`<hostname>.<tailnet>.ts.net:<admin_port>` form, in case a caller addresses the
edge by its FQDN instead of the short MagicDNS label). JSON has no comment
syntax, so this note — and the fact that `caddy.<tailnet>.ts.net:2019` is a
**placeholder you must fill in** before building the image — lives here instead
of inline. Copy the template to the real (gitignored) config and edit *that*,
so your tailnet name never lands in a tracked file:

```sh
cp bootstrap-caddy.json.example bootstrap-caddy.json
```

Replace `<tailnet>` in the copy with your actual tailnet name (visible in the
Tailscale admin console, or via `tailscale status` on any node — it's the part
between the hostname and `.ts.net`). If you changed `caddy.hostname` away from
the default `caddy`, also replace the literal `caddy:2019` / `caddy.<tailnet>...`
entries with your chosen hostname. `fly deploy` (and `docker build`) bake this
filled-in `bootstrap-caddy.json` into the image.

## Building and deploying

This directory doesn't build or deploy itself — see
[`docs/caddy-edge.md`](../../docs/caddy-edge.md) for the full sequence
(Tailscale ACL and auth key, `fly launch`, volume, dedicated IPv4, secrets,
deploy, DNS, first-publish smoke test). In short, once `fly.toml` exists
(copied from `fly.toml.example` and filled in, or generated by `fly launch`
and reconciled against it), `bootstrap-caddy.json` exists (copied from
`bootstrap-caddy.json.example` and filled in, per the section above), and
`TS_AUTHKEY` is set via `fly secrets set`:

```sh
fly deploy
```

## What is / isn't verifiable in this repo's environment

**Verified here:** `bootstrap-caddy.json.example` is valid JSON
(`python3 -m json.tool`) and its `server_name`/`admin_port`/`listen` values
match `internal/config`'s `CaddyConfig` defaults (`tailport`/`2019`);
`entrypoint.sh` passes `bash -n` syntax check; the Dockerfile, entrypoint,
bootstrap config, and `fly.toml.example` are internally consistent with each
other and with the route contract in `internal/caddyedge`
(`@id: tailport-<hostname>`, plain-`http` backend dial, explicit
`header_up Host {label}:{port}`, `terminal: true`). This host has `podman`
(shimmed as `docker`) but no Fly.io account, so `docker build .` in this
directory was run for real (with `bootstrap-caddy.json` first created from the
template — the real file is now gitignored, so a fresh clone must
`cp bootstrap-caddy.json.example bootstrap-caddy.json` before the build's `COPY`
can find it): both build stages complete, the multistage copy of
`tailscale`/`tailscaled` off the official Tailscale image onto `caddy:2-alpine`
succeeds, `bootstrap-caddy.json`/`entrypoint.sh` copy in and `chmod +x` cleanly,
and the image commits and tags successfully. That test image was then deleted;
it was never run.

**Deliberately not run, and not verified here:** the container itself. This
host has no `caddy` binary and no Fly.io account, and — per this task's
explicit instruction — no `caddy` process (inside this image or otherwise)
was started, including a one-shot `caddy validate`. So while the image
*builds*, nothing has confirmed that a real `caddy run --resume` actually
*accepts* `bootstrap-caddy.json.example` (as opposed to it merely being well-formed
JSON matching Caddy's documented schema), and nothing has been deployed to
Fly. Real-`caddy` acceptance is kata v1z5's opt-in, CI-only real-`caddy`
integration test's job, not this task's; the actual Fly deploy is a step
only the maintainer can do (see `docs/caddy-edge.md`'s own verification
note).
