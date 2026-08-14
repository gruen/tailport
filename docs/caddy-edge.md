# Deploying a Caddy edge

Runbook for standing up the **Caddy edge**: a small Fly.io app (tailscaled +
Caddy) that lets tailport publish a local port to a custom public hostname —
`https://app.example.com`, no port in the URL, no `*.ts.net` — over your
tailnet (the `P` key, kata v1z5). Short and skimmable, written for someone
who has never used Caddy or Fly.io before — cross-reference
[`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md) for
what each file does and the root [README](../README.md#configuration) for
the `caddy.*` config fields this deploy has to line up with.

**This is a one-time (or occasional) operator task**, separate from
day-to-day tailport use — everything else (`tailscale serve`/`space`,
Funnel/`p`) needs no edge at all. Do it once per edge, not once per
tailport machine: several tailport computers can publish through the same
edge (see step 3).

`example.com`, `<tailnet>`, and `<your-fly-app-name>` below are
placeholders throughout — substitute your own domain, tailnet name (from the
Tailscale admin console or `tailscale status`), and Fly app name. Nothing in
this repo ever holds your real values — see
[`packaging/caddy-edge/fly.toml.example`](../packaging/caddy-edge/fly.toml.example)'s
header and `.gitignore`.

## 1. Tailscale ACL and auth key

**Order matters here: the ACL change goes first.** Tailscale won't let a
node register under a tag that isn't already owned in the policy file, so
minting the key before the ACL exists just means the edge fails to come up
when you deploy it.

1. In the [Tailscale admin console](https://login.tailscale.com/admin/acls),
   add a `tagOwners` entry for a tag you'll use for this edge (e.g.
   `tag:tailport-edge`):

   ```json
   "tagOwners": {
       "tag:tailport-edge": ["autogroup:admin"],
   },
   ```

   Adjust the owner (`autogroup:admin`, a specific user, or a group) to
   whoever should be allowed to issue keys under this tag. Save the policy.

2. Generate an auth key (admin console → **Settings → Keys → Generate auth
   key**) with:
   - **Reusable** — yes (the edge may need to re-authenticate after a
     volume reset without you minting a fresh key each time).
   - **Ephemeral** — **no**. An ephemeral node is deregistered the moment it
     disconnects; the edge is meant to be a persistent, always-on tailnet
     member.
   - **Tags** — `tag:tailport-edge` (or whatever you added above). Tagging
     the key is what gets you a device with **key expiry disabled**
     automatically — an untagged node's key expires periodically and would
     eventually knock the edge off the tailnet with no interactive way to
     reauth it.

   Copy the resulting `tskey-auth-...` value — you'll hand it to Fly as a
   secret in step 2, never into a file in this repo.

## 2. Fly.io: app, volume, IP, secret, deploy

From [`packaging/caddy-edge/`](../packaging/caddy-edge/):

```sh
fly launch --no-deploy   # generates the real fly.toml; answer "no" to any
                          # prompt that would deploy before the volume/secret
                          # below exist. Reconcile the generated fly.toml
                          # against fly.toml.example -- keep the raw-TCP
                          # [[services]] blocks and the absence of a service
                          # for the admin port.

fly volumes create caddy_data --region <fly-region> --size 1
# Holds tailscaled's node state and Caddy's autosaved live config (routes +
# ACME certs) -- see entrypoint.sh. 1 GB is generous for this workload.

fly ips allocate-v4
# DEDICATED, not shared: ~$2/mo (waived under Fly's $5/mo free allowance for
# a small deployment). Required because Caddy terminates TLS itself on raw
# TCP passthrough -- Fly's free shared IPv4 only supports Fly's own
# HTTP(S)-terminating proxy on 80/443, which this deployment deliberately
# bypasses.
fly ips allocate-v6
# Free, and dedicated by default.

fly secrets set TS_AUTHKEY=tskey-auth-...
# The key from step 1. Fly-side only -- never in fly.toml, the Dockerfile,
# or any committed file.

fly deploy
```

Watch `fly logs` during the first deploy: `entrypoint.sh` prints each stage
(`starting tailscaled`, `tailscale up`, `tailscale serve`, `exec caddy run`)
so a stall is easy to localize. A hang before "tailscale up" usually means
`TS_AUTHKEY` is missing or wrong; a hang after it usually means the ACL
change from step 1 hasn't propagated or the tag isn't owned correctly.

Confirm the node joined: it should appear in the Tailscale admin console's
device list, tagged, with the hostname you expect (`caddy` by default —
see [`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)
for what has to agree with tailport's `caddy.hostname`).

## 3. DNS

Point your domain at the edge's dedicated IPs (`fly ips list` to see them):

- **A** record → the dedicated IPv4.
- **AAAA** record → the IPv6.

**State this plainly because it trips people up: `*.example.com` does NOT
cover `a.b.example.com`.** A DNS wildcard matches exactly one additional
label — `*.example.com` matches `foo.example.com` but not
`bar.baz.example.com`. If you intend to publish nested hostnames (e.g.
tailport's prefill often nests process/label under a base like
`dev.apps.example.com`), each nesting level needs its own wildcard or an
explicit record:

- `*.example.com` → covers `foo.example.com`, not `foo.bar.example.com`.
- `*.apps.example.com` → covers `foo.apps.example.com`, not
  `foo.bar.apps.example.com`.

Pick the base you'll set as `caddy.domain` in tailport's config (see the
root README's [Configuration](../README.md#configuration) section) and make
sure your wildcard depth matches how you actually intend to publish.

DNS propagation can take minutes to hours depending on your provider and
prior TTLs; `dig +short A app.example.com` (or your OS's equivalent) to
confirm before moving on.

## 4. First-publish smoke test

Do this once, after the edge is deployed, DNS points at it, and you've
published at least one port from a tailport-managed backend machine (press
`P`). It deliberately checks **two separate things**, so a failure tells you
which half broke:

```sh
fly ssh console
# now on the edge itself:

tailscale ping <label>
# Confirms the edge can resolve the backend's short MagicDNS label AND
# reach it over the tailnet. If this fails, the problem is tailnet
# connectivity/DNS -- nothing Caddy-specific yet.

curl -H "Host: <label>:<port>" http://<label>:<port>/
# Confirms the Host rewrite Caddy's route performs (header_up Host
# {label}:{port}) actually reaches Tailscale Serve on the backend and gets
# a response. If `tailscale ping` succeeded but this doesn't, the problem
# is specific to the backend's `tailscale serve` mapping or Host handling
# -- not the edge's tailnet connectivity.
```

Then, from a machine that is **not** on your tailnet (confirming the public
path end to end, cert included):

```sh
curl -I https://<published-hostname>/
```

A `200`/`3xx` with a valid certificate here means DNS, the dedicated IP,
Caddy's automatic HTTPS, the route, the Host rewrite, and the backend are
all correctly wired together.

## 5. Troubleshooting

**Certificate issuance fails or never completes.** Automatic HTTPS needs
`:80` (ACME HTTP-01 challenge) and `:443` reachable from the public internet
on the edge's dedicated IPv4/IPv6 — which is exactly what
`fly.toml`'s raw-TCP `[[services]]` blocks provide. Check, in order: DNS
actually resolves to the edge's IPs yet (step 3); the dedicated IPv4 was
allocated, not left shared (step 2); `fly logs` for ACME errors (rate
limits, DNS not yet propagated — Caddy retries with backoff, so a transient
failure often self-heals).

**tailport reports the admin API returned `403`.** The `Host` header on the
request tailport made isn't in `bootstrap-caddy.json`'s `admin.origins`.
Confirm you replaced the `<tailnet>` placeholder before building the image
(see [`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)),
and that `caddy.hostname` in tailport's config matches what's actually
registered on the tailnet (`tailscale status` on the edge, or the admin
console).

**Edge unreachable** (tailport reports it can't reach the admin API at
all, not a `403`). Check, in order: `fly status` / `fly logs` — is the
Machine actually running? `tailscale status` — is the edge listed and
online on the tailnet? From the tailport machine, `tailscale ping
<caddy.hostname>` — does the edge resolve and respond at all, independent
of Caddy? Only once basic tailnet reachability is confirmed is a
Caddy-specific admin-API problem worth chasing.

**A published route 502s → does the edge resolve the short label?** Caddy
dials backends by short MagicDNS label (`<label>:<port>`), which depends on
the edge's resolver carrying `search <tailnet>.ts.net` — stock `tailscaled`
sets this up in `/etc/resolv.conf` when `--accept-dns=true` (the default;
`entrypoint.sh` passes it explicitly). From `fly ssh console` on the edge:

```sh
cat /etc/resolv.conf       # look for: search <tailnet>.ts.net
tailscale ping <label>     # does the short label resolve and respond at all?
```

If the search domain is missing or `tailscale ping <label>` fails here even
though the backend is otherwise fine, the edge's own tailnet DNS setup is
the problem — this is what step 4's smoke test exists to catch before it
ever surfaces as a confusing 502 to an end user.

## 6. Updating the edge

```sh
fly deploy
```

from `packaging/caddy-edge/`, after pulling any changes to this directory.
Because Caddy runs with `--resume`, its **live** config — which is where
tailport's published routes and issued certs actually live, not
`bootstrap-caddy.json` — autosaves onto the volume and is what a restart
resumes from. `bootstrap-caddy.json` is only ever consulted on a true first
boot, before anything has been autosaved. A normal `fly deploy` (new image,
same volume) does **not** lose already-published routes.

To deliberately reset the edge (wipe every route and cert and start clean
from `bootstrap-caddy.json`) you have to either clear the autosave file on
the volume (`fly ssh console`, remove
`$XDG_CONFIG_HOME/caddy/autosave.json`) or destroy and recreate the volume
outright. Either way, every previously published route is gone and each
backend has to republish (press `P` again) — tailport itself keeps no
per-port publish state to restore from; Caddy's live config is the only
source of truth (see kata v1z5's Architecture notes).

## A note on resilience

**This deployment is one Fly Machine with one attached volume — there is no
high availability, by design, and that's stated plainly rather than glossed
over.** A crashed Machine restarts and reattaches to the same volume, so
state survives an ordinary restart. But there is no redundancy: if the
volume itself is lost, or the app is destroyed, every published route and
every issued TLS certificate goes with it, and — same as an intentional
reset above — each backend must republish from scratch. This is judged an
acceptable trade-off for a personal tool serving a handful of machines, not
a production SLA. If you want a recovery story beyond "republish," Fly
volume snapshots (`fly volumes snapshots create`) are the mechanism, but
setting up a snapshot schedule is outside this runbook's scope.
