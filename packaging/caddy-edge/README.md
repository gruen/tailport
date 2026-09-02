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
| [`entrypoint.sh`](./entrypoint.sh) | Container entrypoint: starts `tailscaled`, joins the tailnet, exposes Caddy's admin API tailnet-only via `tailscale serve`, sed-templates the live Caddy config from env (see below), then `exec`s `caddy run --resume`. |
| [`bootstrap-caddy.json.example`](./bootstrap-caddy.json.example) | Template for Caddy's first-boot admin config: an empty `tailport` HTTP server on `:443`/`:80`, plus a widened `admin.origins` list with a placeholder token (see below). Tracked as-is and `COPY`ied straight into the image — nothing to fill in before building. `entrypoint.sh` fills the token in from env at container boot. |
| [`fly.toml.example`](./fly.toml.example) | Template for a Fly.io deployment: raw-TCP passthrough on 80/443, the state volume, and deliberately no service for the admin port. |
| [`edge.env.example`](./edge.env.example) | Template for the deploy variables (`APP`/`REGION`/`DOMAIN`/`HOSTNAME`/`TAG`) the runbook's scripted appendix uses. Copy to a gitignored `edge.env` and `source` it instead of retyping them each shell. Not secrets — `TS_AUTHKEY` stays in `fly secrets`. |

## How this maps to tailport's `caddy.*` config

tailport's own config (`~/.config/tailport/config.yaml`, documented in the
root [README](../../README.md#configuration)) has a `caddy:` block with four
fields that must agree with what this edge is actually running as:

| tailport config field | Default | What it must match here |
| --- | --- | --- |
| `caddy.hostname` | `caddy` | The `--hostname` `tailscale up` registers (`TS_HOSTNAME` in `entrypoint.sh`, default `caddy`). This is the edge's own tailnet identity — how tailport *finds* the admin API — not any published route's public hostname. |
| `caddy.admin_port` | `2019` | `entrypoint.sh`'s `CADDY_ADMIN_PORT` — templated at boot into the live config's `admin.listen`/`admin.origins` ports (see below) — and the `tailscale serve --http=<port> <port>` mapping it sets up. |
| `caddy.server_name` | `tailport` | `bootstrap-caddy.json.example`'s `apps.http.servers.<server_name>` key — the one shared Caddy HTTP server every tailport computer publishing through this edge writes routes into. |
| `caddy.domain` | `""` (blank) | Not part of this image at all — it's the public base domain tailport builds *publish* hostnames from (`<prefix>.<domain>`), pointed at this edge's DNS (see the runbook's DNS step). Unrelated to the edge's own tailnet hostname above. |

`hostname` and `admin_port` are both plain env vars on the edge side
(`TS_HOSTNAME`/`CADDY_ADMIN_PORT` in `entrypoint.sh`, set via `fly.toml`'s
`[env]` if you move either off its default) — `entrypoint.sh` derives the live
`admin.origins`/`admin.listen` values from them at boot, so there is nothing to
hand-edit in a config file for either. `server_name` is the one field that
still lives directly in the template: if you change it away from `tailport`,
edit the `apps.http.servers` key in `bootstrap-caddy.json.example` itself (safe
to do — the template carries no tailnet-specific data). None of these three are
read from tailport's own config file; this image and tailport's config are two
independent places kept in sync by hand (or, for `hostname`/`admin_port`, by
matching env vars).

## Why `admin.origins` is widened, and how it's filled in

Caddy's admin API checks the incoming request's `Host` header against an
allow-list (`admin.origins`) and returns `403` for anything else — a
DNS-rebinding mitigation. By default that list is just `localhost:2019` /
`127.0.0.1:2019`. But tailport reaches this admin API over the tailnet at
`http://<caddy.hostname>:<caddy.admin_port>` (e.g. `http://caddy:2019`), so
the `Host` header on that request is `caddy:2019`, not `localhost:2019` —
which would 403 against the unmodified default.

The tracked template widens `admin.origins` with a placeholder token,
`__TAILPORT_ADMIN_ORIGIN__`, alongside the `localhost`/`127.0.0.1` entries — no
tailnet name, no hostname, valid JSON exactly as committed. `entrypoint.sh`
fills that token in from env, in-shell, right before `caddy run`: every literal
`:2019` in the template — `admin.listen` plus the `localhost`/`127.0.0.1`
origins — is repointed at the real admin port, and the token becomes
`$TS_HOSTNAME:$CADDY_ADMIN_PORT` (tailport's actual admin `Host`) — both in the
same pass (a no-op if `CADDY_ADMIN_PORT` is left at its default). See the `sed`
step and its comment in `entrypoint.sh` for the exact substitution and why the
order (`:2019` port rewrite first, token injection second) matters — the token
already expands to `<host>:<CADDY_ADMIN_PORT>`, so injecting it last keeps the
`:2019` rewrite from mangling it when the admin port itself begins with `2019`.

There is nothing to fill in before building the image — the tracked
`bootstrap-caddy.json.example` is exactly what `docker build`/`fly deploy` bake
in, unmodified. The env-derived value is correct on first boot (`$TS_HOSTNAME`
is deterministic, no `tailscale status` race), and because Caddy's own
autosave then carries that live config forward across restarts (`--resume`,
see [`docs/caddy-edge.md`](../../docs/caddy-edge.md)'s "Updating the edge"
section), it stays correct without re-templating on every boot.

**Known limitation:** if you later change `caddy.hostname`/`TS_HOSTNAME` **or**
`caddy.admin_port`/`CADDY_ADMIN_PORT` *after* the edge has already booted once,
the autosave still holds the old values — the entrypoint only templates a fresh
`admin.listen`/`admin.origins` into a true first-boot config, never into the
resumed one. A stale hostname 403s tailport's admin requests; a stale admin
port leaves `tailscale serve` forwarding to the new port while resumed Caddy
still listens on the old one, so the admin API goes unreachable. Recovery for
either is the existing edge-reset procedure (docs/caddy-edge.md's "Updating the
edge" section: clear the autosave file or recreate the volume), not something
this templating re-does automatically.

## Building and deploying

This directory doesn't build or deploy itself — see
[`docs/caddy-edge.md`](../../docs/caddy-edge.md) for the full sequence
(Tailscale ACL and auth key, `fly launch`, volume, dedicated IPv4, secrets,
deploy, DNS, first-publish smoke test). In short, once `fly.toml` exists
(copied from `fly.toml.example` and filled in, or generated by `fly launch`
and reconciled against it) and `TS_AUTHKEY` is set via `fly secrets set`:

```sh
fly deploy
```

## What is / isn't verifiable in this repo's environment

**Verified here:** `bootstrap-caddy.json.example` is valid JSON
(`python3 -m json.tool`) and its `server_name`/`admin_port`/`listen` values
match `internal/config`'s `CaddyConfig` defaults (`tailport`/`2019`);
`entrypoint.sh` passes `bash -n` syntax check; the Dockerfile, entrypoint,
bootstrap template, and `fly.toml.example` are internally consistent with each
other and with the route contract in `internal/caddyedge`
(`@id: tailport-<hostname>`, plain-`http` backend dial, explicit
`header_up Host {label}:{port}`, `terminal: true`). `entrypoint.sh`'s `sed`
templating step was hand-run against the tracked `.example` for two env cases
— `TS_HOSTNAME=caddy CADDY_ADMIN_PORT=2019` (the defaults) and
`TS_HOSTNAME=edge CADDY_ADMIN_PORT=9999` (a non-default hostname and port) —
and both outputs were confirmed valid JSON (`python3 -m json.tool`) with
`admin.listen` and every `admin.origins` entry correctly repointed at the
chosen port, and no double-substitution. This host has `podman` (shimmed as
`docker`) but no Fly.io account, so `docker build .` in this directory was run
for real against the *current* Dockerfile: no pre-build fill-in step is needed
any more — the tracked `bootstrap-caddy.json.example` is `COPY`ied as-is, so a
fresh clone builds with no manual step first. Both build stages complete, the
multistage copy of `tailscale`/`tailscaled` off the official Tailscale image
onto `caddy:2-alpine` succeeds, the template/`entrypoint.sh` copy in and
`chmod +x` cleanly, and the image commits and tags successfully. That test
image was then deleted; it was never run.

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
