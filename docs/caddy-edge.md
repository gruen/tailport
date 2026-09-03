# Deploying a Caddy edge

Runbook for standing up the **Caddy edge**: a small always-on host running
tailscaled + Caddy that lets tailport publish a local port to a custom public
hostname — `https://app.example.com`, no port in the URL, no `*.ts.net` — over
your tailnet (the `p` key, kata v1z5; swapped from `P` under vzj4).

**The edge is not tied to any one provider.** tailport only ever speaks Caddy's
admin API over the tailnet, so *any* host that meets the [requirements
below](#requirements-any-host) works — a cloud VM, a VPS, a home server, a
Raspberry Pi. This runbook uses **Fly.io as the reference recipe** because it
makes the "public IP + tailscaled + a persistent volume + a container" bundle
cheap and turnkey; [Self-hosting / other
providers](#self-hosting--other-providers) maps every step to a box you manage.

Short and skimmable, written for someone who has never used Caddy (or Fly.io)
before — cross-reference
[`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md) for
what each file does and the root [README](../README.md#configuration) for
the `caddy.*` config fields this deploy has to line up with.

**This is a one-time (or occasional) operator task**, separate from
day-to-day tailport use — everything else (`tailscale serve`/`space`,
Funnel/`P`) needs no edge at all. Do it once per edge, not once per
tailport machine: several tailport computers can publish through the same
edge (see step 4).

`example.com` and `<your-fly-app-name>` below are placeholders throughout —
substitute your own domain and Fly app name. Nothing in this repo ever holds
your real values — see
[`packaging/caddy-edge/fly.toml.example`](../packaging/caddy-edge/fly.toml.example)'s
header and `.gitignore`.

**Just want the whole thing to copy-paste?** The [scripted
appendix](#appendix-the-whole-deploy-as-one-scripted-sequence) at the end is
every step below collapsed into one parameterized sequence — fill a few shell
variables and run it, each command explained inline. The numbered sections
remain the reference for *why* each piece is there.

## Requirements (any host)

The edge is a contract, not a Fly thing. Whatever you run it on has to supply
all five of these — that's the whole list:

| # | Requirement | Why it's needed |
| - | ----------- | --------------- |
| 1 | A **public IP** with **`:80` and `:443` reaching Caddy directly** — raw TCP, nothing in front terminating TLS | Caddy terminates TLS and answers the ACME `HTTP-01` challenge itself; anything intercepting the handshake first breaks issuance and passthrough |
| 2 | **tailscaled** on that host, joined to your tailnet under the edge tag | how the edge reaches your backends, and how tailport reaches the edge's admin API — all tailnet-only, never public |
| 3 | **Caddy ≥ 2.5.2**, admin API reachable **over the tailnet only** (never the public interface) | the version floor is for concurrency safety (see the Caddy ≥ 2.5.2 requirement below); the admin API has no auth of its own, so tailnet-only + the ACL *is* the access control |
| 4 | **DNS** for your publish domain (a wildcard, or per-host records) pointing at that public IP | so published hostnames resolve to the edge and ACME can validate them |
| 5 | **Persistent storage** for tailscaled's state and Caddy's autosaved live config + issued certs | so published routes and TLS certs survive a restart instead of re-issuing every boot |

Meet those five and the publish flow can't tell the difference between Fly and
anything else. The Fly recipe in §§1–7 is one concrete way to satisfy them;
[Self-hosting / other providers](#self-hosting--other-providers) is the same
five requirements on a box you manage.

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

   `tagOwners` alone only decides who can *mint a key* for this tag — it
   grants the edge no actual network reachability. Two more rules belong in
   the same policy, in **both directions**, or the edge either can't reach
   your backends or is more exposed than you think:

   - **The edge needs to reach the backend tailport machine(s) it proxies
     to.** On a tailnet whose policy already defaults to "everyone reaches
     everyone" this may already work, but don't assume it — a locked-down
     tailnet denies by default, and the reverse-proxy hop (`header_up Host
     {label}:{port}`, dialing the backend's short MagicDNS label) will fail
     with nothing more specific than a 502 if `tag:tailport-edge` has no
     path to the backend's ports.
   - **Only trusted nodes should be able to reach the edge's admin API on
     `caddy.admin_port` (`:2019` by default).** This is the more important
     rule of the two. Caddy's admin API has **no authentication of its
     own** — it's guarded only by (a) whether a caller can reach the port
     over the tailnet at all, and (b) a `Host`-header allow-list
     (the live config's `admin.origins`, derived from env at boot -- see
     [`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)),
     which is an anti-DNS-rebinding check, not an identity check — anyone
     who can reach the port and send a matching `Host` header can add,
     remove, or rewrite every published route, including turning off
     basic auth on someone else's route. On a tailnet with an open default
     policy, that means *any* tailnet member, not just the tailport
     machines you intend to administer it from.

   Tailscale's policy file is [HuJSON](https://tailscale.com/kb/1018/acls)
   (JSON plus comments and trailing commas). Tailscale's current syntax is
   **`grants`**; the older `acls` array still works but is frozen (no new
   features), and new tailnets default to `grants`. What you actually need
   depends on your tailnet:

   - **Open (default) tailnet — `tagOwners` above is all you need to get
     running.** A new tailnet's starter policy already lets every node reach
     every other, so both reachability rules below are already satisfied; add
     the `tagOwners` entry, save, and go mint the key. (Heads-up: that same
     openness means the edge's admin API on `:2019` is reachable by *every*
     tailnet member — see the hardening note after the example.)
   - **Locked-down tailnet (default-deny or scoped) — add the two grants
     too**, or the edge can't reach your backends (rule 1) and/or the wrong
     people can manage it (rule 2).

   Current syntax (`grants`) — adapt against
   [Tailscale's grants reference](https://tailscale.com/docs/reference/syntax/grants)
   rather than pasting verbatim:

   ```json
   {
     "tagOwners": {
       "tag:tailport-edge":    ["autogroup:admin"],
       "tag:tailport-backend": ["autogroup:admin"], // the tag your backend machine(s) carry
     },

     "grants": [
       // ... whatever grants your tailnet already has ...

       // 1) Let the edge reach the backend tailport machine(s) it proxies to,
       //    on the ports they actually serve. Scope dst to your real
       //    backend(s) -- a "tailport-backend" tag here -- not every port.
       {
         "src": ["tag:tailport-edge"],
         "dst": ["tag:tailport-backend"],
         "ip":  ["tcp:8080", "tcp:8443", "tcp:9000"],
       },

       // 2) Grant ONLY specific people the ability to manage this edge --
       //    i.e. reach Caddy's admin API on :2019, which is add/remove/rewrite
       //    of every published route (it has no auth of its own, so this grant
       //    IS the access control). List the exact users, a group, or a tag on
       //    the machines they drive tailport from. Do NOT use
       //    "autogroup:member" or "*" here -- that's the over-broad grant this
       //    rule exists to prevent.
       {
         "src": ["alice@example.com", "bob@example.com"], // or ["group:caddy-admins"]
         "dst": ["tag:tailport-edge"],
         "ip":  ["tcp:2019"],
       },
     ],
   }
   ```

   `grants` differ from the older `acls` form in exactly two ways: there is no
   `"action": "accept"` (grants always allow), and the port moves off the `dst`
   into its own `ip` field (`"dst": ["tag:x"]` + `"ip": ["tcp:2019"]`, not
   `"dst": ["tag:x:2019"]`). If your policy still uses the older `acls` array,
   the equivalent is:

   ```json
   "acls": [
     { "action": "accept", "proto": "tcp", "src": ["tag:tailport-edge"],
       "dst": ["tag:tailport-backend:8080,8443,9000"] },
     { "action": "accept", "proto": "tcp", "src": ["alice@example.com", "bob@example.com"],
       "dst": ["tag:tailport-edge:2019"] },
   ],
   ```

   **On an already-open tailnet, rule 2 alone does not lock down `:2019`.**
   Grants and ACLs are additive — any matching rule *allows*, there is no
   `deny` — so a narrow "only alice+bob" grant does not subtract the starter
   policy's broad `src: ["*"] → dst: ["*"]` reach to the admin port. To
   actually limit who can manage the edge you must narrow that existing broad
   grant (or move off allow-all to explicit grants) so it no longer covers
   `tag:tailport-edge:2019`; rule 2 then defines who *does* get in. Check what
   your policy already allows before assuming rule 2 is enough on its own.

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

This is the **reference recipe** — Fly's concrete way to satisfy
[requirements](#requirements-any-host) 1, 3, and 5 (the public IP, the
tailscaled + Caddy container, and a persistent volume) in one place. On a host
you manage you provision those differently; see [Self-hosting / other
providers](#self-hosting--other-providers). Steps 1, 3, and 4 are not
Fly-specific and apply verbatim wherever the edge runs.

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
(`starting tailscaled`, `tailscale up`, `tailscale serve`, `templating`, `exec
caddy run`) so a stall is easy to localize. A hang before "tailscale up" usually means
`TS_AUTHKEY` is missing or wrong; a hang after it usually means the ACL
change from step 1 hasn't propagated or the tag isn't owned correctly.

Confirm the node joined: it should appear in the Tailscale admin console's
device list, tagged, with the hostname you expect (`caddy` by default —
see [`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)
for what has to agree with tailport's `caddy.hostname`).

**Do not run `fly certs add` (or set any Fly certificate).** Caddy owns TLS
here: the `[[services]]` blocks are raw TCP passthrough, so Fly never sees the
handshake — a Fly cert would do nothing, and Fly's proxy can't terminate on the
same `:443` Caddy does. Each published hostname's certificate is obtained and
renewed automatically by Caddy through its default ACME issuers (Let's Encrypt,
with ZeroSSL as fallback) when the route for that hostname is first published —
provided its DNS (next step) resolves to this edge and `:80`/`:443` are
reachable from the public internet. (This is Caddy's normal automatic HTTPS on
route provisioning, not its separate On-Demand TLS feature, which this edge does
not enable.) It's a per-hostname cert, not a wildcard (see §6 if issuance
stalls).

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

Pick the base you'll set as `caddy.domain` on each publishing machine (step 4
below) and make sure your wildcard depth matches how you actually intend to
publish.

DNS propagation can take minutes to hours depending on your provider and
prior TTLs; `dig +short A app.example.com` (or your OS's equivalent) to
confirm before moving on.

## 4. Point tailport at the edge

Steps 1–3 stand up the shared edge once. This step is per-machine: every
tailport computer that will publish needs to know where the edge is and what
base domain to build public hostnames from. That lives in tailport's own
config — `~/.config/tailport/config.yaml` (or `$XDG_CONFIG_HOME/tailport/...`) —
under a `caddy:` block:

```yaml
caddy:
    hostname: caddy            # the edge's PRIVATE tailnet name; tailport reaches its admin API here
    domain: apps.example.com   # the base domain from step 3 -- public hostnames are built from this
    server_name: tailport      # must match on every computer publishing through this same edge
    admin_port: 2019
```

Set **`domain`** to the base whose DNS you pointed at the edge in step 3, at the
wildcard depth you actually publish at (`*.apps.example.com` → `domain:
apps.example.com`). While it's blank the background published-state poll stays
off (zero cost until you opt in); you don't have to hand-edit it, though —
pressing `p` on a port with a blank `domain` prompts for it inline and saves it
for you (a targeted write that preserves the rest of the file), then continues
the publish.

tailport writes this block with commented defaults the first time it saves the
config, so normally you only edit the `domain:` line. **If your `config.yaml`
predates the publish feature the block won't be there yet** — trigger one save
with any change that writes the file (favouriting or labelling a port), or
paste the block above in by hand, then set `domain:`. Publishing can also seed
it: pressing `p` with a blank `domain` captures it inline and saves it rather
than refusing. Full field reference: the root README's
[Configuration](../README.md#configuration) section.

`hostname` is the edge's own short MagicDNS name (default `caddy`), used only so
tailport can find its admin API over the tailnet — it is unrelated to any
published *public* hostname. Hand-editing it is no longer the only path: on a
fresh setup (blank `domain`), pressing `p` also prompts for `hostname` first
(prefilled with the current value, so accepting the default is a no-op),
*before* the `domain` prompt above — it's needed to reach the admin API at all.
Already-configured setups aren't re-prompted. `server_name` must be identical
on every tailport computer sharing this edge (it selects the shared routes
array); it does not identify the source machine.

**Known limitation:** the edge derives its admin API's identity — both the
allow-list (`admin.origins`) and the listener/origin *port* — from
`caddy.hostname`/`TS_HOSTNAME` and `caddy.admin_port`/`CADDY_ADMIN_PORT`
automatically on its *first* boot only (see
[`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)'s
`admin.origins` section) — Caddy's autosave then carries those values forward
across every later restart. If you change **either** the hostname **or** the
admin port **after** the edge has already booted once, the autosaved config is
now stale: a changed hostname makes tailport's admin requests 403 on a stale
origin, and a changed admin port leaves Tailscale Serve forwarding to the new
port while resumed Caddy still listens on the old one — the admin API goes
unreachable. Neither is auto-reconciled; recover with the existing edge-reset
procedure in step 7 (clear the autosave file or recreate the volume) so the
edge re-derives its admin identity from the new env on its next,
effectively-first, boot.

## 5. First-publish smoke test

Do this once, after the edge is deployed, DNS points at it, and you've
published at least one port from a tailport-managed backend machine (press
`p`). It deliberately checks **two separate things**, so a failure tells you
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
curl -sS -o /dev/null -w '%{http_code}\n' https://<published-hostname>/
```

This is a GET with the body discarded (`-o /dev/null`) and only the status
code printed (`-w '%{http_code}\n'`) — deliberately not `curl -I` (a HEAD
request): a backend that serves GET but doesn't support HEAD would answer
`405` there, which reads as a broken deployment when it isn't.

For a route published **without** basic auth, a `2xx`/`3xx` status with a
valid certificate here means DNS, the dedicated IP, Caddy's automatic
HTTPS, the route, the Host rewrite, and the backend are all correctly
wired together.

For a route published **with** basic auth (tailport's shared,
bcrypt-hashed `auth_user`/`auth_hash` credential — see the root
[README](../README.md#configuration)), **a `401` on this unauthenticated
request is the expected, correct result, not a failure** — it's proof
Caddy's `http_basic` handler is active and gating the route before the
request ever reaches the backend. Confirm the backend is actually
reachable by retrying with the credential:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' -u '<auth_user>' https://<published-hostname>/
```

Pass only the username to `-u`, with no `:<password>` after it — curl
then prompts for the password interactively instead of taking it on the
command line, where it would land in your shell history and be visible
to other processes on the machine (e.g. `ps`) for as long as it survives
there.

A `2xx`/`3xx` on *this* authenticated request is what confirms the full
chain end to end for a protected route. A `401` on the unauthenticated
request above is expected on its own and not itself evidence of a
problem; something other than `401`/`2xx`/`3xx` on either request (a
`502`, a TLS error, a hang) means work through Troubleshooting below.

## 6. Troubleshooting

**Certificate issuance fails or never completes.** Automatic HTTPS needs
`:80` (ACME HTTP-01 challenge) and `:443` reachable from the public internet
on the edge's dedicated IPv4/IPv6 — which is exactly what
`fly.toml`'s raw-TCP `[[services]]` blocks provide. Check, in order: DNS
actually resolves to the edge's IPs yet (step 3); the dedicated IPv4 was
allocated, not left shared (step 2); `fly logs` for ACME errors (rate
limits, DNS not yet propagated — Caddy retries with backoff, so a transient
failure often self-heals). Do **not** reach for `fly certs add`: that manages
certs for Fly's own TLS-terminating proxy, which this raw-passthrough setup
bypasses — the cert is Caddy's to issue, so the fix is always DNS/reachability,
never a Fly certificate.

**tailport reports the admin API returned `403`.** The `Host` header on the
request tailport made isn't in the live config's `admin.origins`. The edge
derives that list from `$TS_HOSTNAME`/`$CADDY_ADMIN_PORT` at first boot (see
[`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)'s
`admin.origins` section), so a `403` almost always means `caddy.hostname` in
tailport's config doesn't match what's actually registered on the tailnet
(`tailscale status` on the edge, or the admin console) — **or** you changed
`caddy.hostname`/`TS_HOSTNAME` *after* the edge's first boot, in which case the
autosaved live config still has the old origin baked in (see the Known
limitation in step 4 below; recovery is the reset procedure in step 7).

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
the problem — this is what step 5's smoke test exists to catch before it
ever surfaces as a confusing 502 to an end user.

## 7. Updating the edge

```sh
fly deploy
```

from `packaging/caddy-edge/`, after pulling any changes to this directory.
Because Caddy runs with `--resume`, its **live** config — which is where
tailport's published routes and issued certs actually live, not the
boot-time template — autosaves onto the volume and is what a restart resumes
from. `bootstrap-caddy.json.example` (templated by `entrypoint.sh` into a
runtime config from `$TS_HOSTNAME`/`$CADDY_ADMIN_PORT`, see
[`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)) is
only ever consulted on a true first boot, before anything has been
autosaved. A normal `fly deploy` (new image, same volume) does **not** lose
already-published routes.

To deliberately reset the edge (wipe every route and cert, and start clean
from the templated boot config again — also the fix if `admin.origins` is
stale after changing `caddy.hostname`/`TS_HOSTNAME`, per step 4's Known
limitation) you have to either clear the autosave file on the volume
(`fly ssh console`, remove `$XDG_CONFIG_HOME/caddy/autosave.json`) or destroy
and recreate the volume outright. Either way, every previously published
route is gone and each backend has to republish (press `p` again) — tailport
itself keeps no per-port publish state to restore from; Caddy's live config
is the only source of truth (see kata v1z5's Architecture notes).

## Self-hosting / other providers

Everything above is the Fly recipe; this is the *same edge* on a box you manage
— a VPS (Hetzner, DigitalOcean, Linode, Vultr…), a cloud VM, or a home server
with `:80`/`:443` forwarded to it. You're satisfying the exact five
[requirements](#requirements-any-host); only *how* you provision each one
changes. Map the Fly steps to their generic equivalents:

| Fly step | On a host you manage |
| -------- | -------------------- |
| §1 Tailscale ACL + auth key | **Unchanged** — the tailnet is provider-independent. Same tag, same reusable, non-ephemeral key. |
| §2 `fly launch` (the container) | Run the bundled [`Dockerfile`](../packaging/caddy-edge/Dockerfile) + [`entrypoint.sh`](../packaging/caddy-edge/entrypoint.sh) under `docker run`/`docker compose` — they're provider-neutral (reading `TS_AUTHKEY`/`TS_HOSTNAME`/`CADDY_ADMIN_PORT` from the env) — but a real run also needs what Fly supplied implicitly: the `/dev/net/tun` device + `NET_ADMIN` capability for `tailscaled`, published `:80`/`:443`, the persistent volume from the next row, and `TS_AUTHKEY` as a secret (never a committed file). Running the two daemons *directly* under systemd instead means reproducing everything `entrypoint.sh` does — admin-origins templating, `tailscale up` with the tag, `tailscale serve` to expose `:2019` tailnet-only, `caddy run --resume` — so read it first: simply starting both processes is **not** a functional edge. |
| §2 `fly volumes create` | Any persistent path: a bind mount, a named Docker volume, or just a directory on disk — it holds tailscaled state + Caddy's autosave/certs ([requirement 5](#requirements-any-host)). |
| §2 `fly ips allocate-v4` | **Not needed** — you already have a public IP. This step is a Fly quirk: Fly's *shared* IPv4 routes through Fly's own TLS-terminating proxy, so Fly makes you buy a *dedicated* IPv4 to get raw passthrough. A normal host's IP is already direct. |
| §3 DNS | **Unchanged** — point your domain at *this* host's public IP(s) instead of Fly's. |
| §4 Point tailport at the edge | **Unchanged** — `caddy.hostname` is the edge's MagicDNS name whatever it runs on. |
| §5 Smoke test | The definitive end-to-end check — `curl https://<hostname>/` from a machine **off** your tailnet — is provider-agnostic and unchanged. To localize a failure *inside* the edge, run §5's diagnostics there (`docker exec`/`compose exec` into the container, or a host shell for a native systemd deploy — a host-shell probe otherwise tests the host's own `tailscaled`/resolver, not the container's): `tailscale ping` works as-is, but the `curl -H "Host: …"` probe needs a `curl` the minimal image doesn't ship (bash + CA certs only), so run that one from the host or a throwaway container sharing the edge's network. |

Two Fly-specific warnings in this runbook simply don't apply off Fly:

- **The dedicated-IPv4 dance (§2) evaporates.** A self-hosted box's public IP
  already reaches Caddy directly — there's no proxy to route around.
- **"Do NOT `fly certs add`" (§2, §6) is moot** — there's no Fly certificate to
  mis-issue. But the *rule underneath it* holds everywhere: **nothing may sit in
  front of Caddy on `:80`/`:443` terminating TLS.** If your provider or home
  router puts a reverse proxy / load balancer with its own TLS ahead of the
  host, either disable it for those two ports or give Caddy its own IP — Caddy
  must own the handshake, exactly as [requirement
  1](#requirements-any-host) says.

Everything else applies identically no matter where the edge runs: the
admin-API hardening (§1), the Caddy ≥ 2.5.2 floor (below), and the single-node
[resilience caveat](#a-note-on-resilience) — a self-hosted box is likewise one
node with one data directory unless you build redundancy yourself.

## Requirement: Caddy ≥ 2.5.2 (concurrency safety)

tailport's publish, unpublish, and **force-purge** paths write into a **shared**
routes array that every tailport machine — and any hand-authored config — can
also touch. To keep concurrent writers from clobbering each other, every mutation
is guarded by an `If-Match` conditional request against the ETag Caddy returned
for the config it read: a racing edit moves the ETag, Caddy answers `412`, and
tailport re-reads and retries instead of overwriting blind. Force-purge leans on
this doubly — it deletes a route by index or `@id` under the ETag that pinned the
array it read, so a concurrent shift can't make the delete land on the wrong
route.

**`ETag`/`If-Match` first shipped in Caddy v2.5.2.** On an **older** edge Caddy
**ignores `If-Match` entirely** — no `412` is ever returned, the retry loops are
dead code, and concurrent writers (and a force-delete racing an array shift) can
clobber the wrong route silently. So **Caddy ≥ 2.5.2 is required**; tailport now
**refuses** a force-purge outright when the edge returns no ETag rather than issue
an unconditional delete. The bundled edge image pins **`caddy:2-alpine`**
(`packaging/caddy-edge/Dockerfile`), currently well above the floor, so a
from-our-packaging edge already satisfies this — the floor only matters if you
point tailport at a **hand-run or pinned-older** Caddy. (For general security
hygiene, tracking a recent 2.x is recommended regardless.)

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

## Appendix: the whole deploy as one scripted sequence

The seven sections above are the reference — each carries the *why*. This
appendix collapses them into one parameterized flow, for when you've done it
once and want to copy-paste, or just want the whole shape on one screen. Every
value is a shell variable set once up front; the two Tailscale **admin-console**
steps (ACL, key) aren't shell and are marked as such. The `§N` links point back
to the full rationale.

### Set these once (a file you source)

Five values feed the whole appendix. Rather than retyping them into every new
shell, keep them in a file and `source` it — that's what the tracked
`edge.env.example` template is for. Its values:

```sh
APP=my-tailport-edge      # Fly app name — globally unique across all of Fly; pick anything free
REGION=iad                # Fly region id (`fly platform regions`), near your visitors
DOMAIN=example.com        # your public base domain → tailport's caddy.domain (§4)
HOSTNAME=caddy            # the edge's TAILNET name → tailport's caddy.hostname (§4); default `caddy`
TAG=tag:tailport-edge     # the Tailscale ACL tag the edge registers under (§1)
```

Copy it to a gitignored `edge.env`, fill it in, and load it in the shell you
deploy from — re-`source` it whenever you open a new shell:

```sh
cd "$(git rev-parse --show-toplevel)/packaging/caddy-edge"
cp edge.env.example edge.env
$EDITOR edge.env
. ./edge.env              # `source edge.env` in bash
echo "APP=$APP REGION=$REGION DOMAIN=$DOMAIN HOSTNAME=$HOSTNAME TAG=$TAG"
# ↑ all five must be non-empty before you continue.
```

`edge.env` matches the gitignore's `*.env` rule so it can't be committed — keep
it that way: the one real secret, `TS_AUTHKEY`, is set with `fly secrets set`
below, never in this file. (Prefer not to keep a file? Paste the value block
inline instead; those are ordinary shell variables that just vanish when the
shell closes.)

### Tailscale: ACL, then key — admin console (§1)

**ACL first** — Tailscale refuses to register a node under a tag it doesn't yet
own. In the [admin console](https://login.tailscale.com/admin/acls): add a
`tagOwners` entry for `$TAG`. On an **open (default) tailnet that's all you
need here** — skip to the key. On a **locked-down** tailnet also add two
`grants` (§1): (a) let `$TAG` reach the backend machines on the ports they
serve, and (b) grant only the specific users who may manage the edge access to
the admin API (`:2019`) — Caddy's admin API has no auth of its own. Full JSON,
the `grants`↔`acls` mapping, and the "additive, no deny" caveat: §1.

Then **mint an auth key** (Settings → Keys): **reusable**, **non-ephemeral**,
tagged `$TAG` — tagging is what disables key expiry. Copy the `tskey-auth-…`; it
becomes a Fly secret below, never a file in the repo.

### Fly: files, resources, deploy — shell, from `packaging/caddy-edge/` (§2)

```sh
cd "$(git rev-parse --show-toplevel)/packaging/caddy-edge"

# fly.toml is gitignored — generate it from the template with your app + region:
cp fly.toml.example fly.toml
sed -i -e "s|<your-fly-app-name>|$APP|" -e "s|<fly-region>|$REGION|g" fly.toml

# Only if HOSTNAME isn't the default `caddy`: register the node under it, or
# tailport can't find the admin API at http://$HOSTNAME:2019 :
if [ "$HOSTNAME" != caddy ]; then
  printf '\n[env]\n  TS_HOSTNAME = "%s"\n' "$HOSTNAME" >> fly.toml
fi

# Nothing to generate for bootstrap-caddy.json.example -- it's tracked as-is
# (a placeholder token in admin.origins, no tailnet/hostname baked in) and
# COPYied straight into the image. entrypoint.sh fills the token in from
# TS_HOSTNAME/CADDY_ADMIN_PORT at container boot -- see
# packaging/caddy-edge/README.md's admin.origins section.

fly apps create "$APP"                                     # the Fly app itself
fly volumes create caddy_data --region "$REGION" --size 1  # persists tailscaled state + Caddy's live config/certs
fly ips allocate-v4                                        # DEDICATED IPv4 (~$2/mo). OMIT for IPv6-only.
fly ips allocate-v6                                        # free, dedicated by default
fly secrets set TS_AUTHKEY=tskey-auth-...                  # the key from the ACL step (Fly-side only)
fly deploy                                                 # builds the image and boots it (entrypoint templates admin.origins from env)
# NO `fly certs add` / no Fly certificate: raw TCP passthrough means Caddy issues
# and renews each published hostname's TLS cert itself via its default ACME
# issuers (Let's Encrypt, ZeroSSL fallback) when the route is published.
fly logs                                                   # follow: tailscaled → up → serve → templating → caddy run
fly ips list                                               # the v4/v6 you point DNS at
```

> Unlike `fly.toml`, `bootstrap-caddy.json.example` needs no gitignored,
> filled-in copy any more — the edge derives its admin identity from env at
> boot instead (previous `bootstrap-caddy.json` files from that older flow
> are vestigial; safe to delete, see
> [`packaging/caddy-edge/README.md`](../packaging/caddy-edge/README.md)).

### DNS (§3)

Point a wildcard at the addresses `fly ips list` printed. Print the exact record
names to type into your DNS provider with:

```sh
echo "*.$DOMAIN   A      <dedicated-v4>   # omit for IPv6-only"
echo "*.$DOMAIN   AAAA   <v6>"
```

A wildcard matches exactly one label: `*.$DOMAIN` covers `foo.$DOMAIN` but not
`foo.bar.$DOMAIN`, so `*.$DOMAIN` suffices **only** for hostnames exactly one
label beneath `$DOMAIN`. The publish hostname is editable in the `p` flow and
its prefill can nest (e.g. `<label>.<group>.$DOMAIN`), so if you publish nested
names, add a matching wildcard/record for each level — §3 covers this in full.

### tailport: point at the edge, then verify (§4, §5)

On **each** machine that will publish, set in `~/.config/tailport/config.yaml`:
`caddy.domain: <your DOMAIN>` (and `caddy.hostname: <your HOSTNAME>` if you
changed it off `caddy`). Restart tailport, publish a port with `p` — note the
**exact** hostname it confirms (that's what DNS must cover and what you test,
not an assumed `<label>.$DOMAIN`) — then from a host **not** on your tailnet:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' https://<the-hostname-p-confirmed>/
```

`2xx`/`3xx` with a valid certificate means the whole chain — DNS, the dedicated
IP, Caddy's automatic HTTPS, the route, the Host rewrite, the backend — is wired
correctly. A `401` is the *expected* answer for a route you published behind
basic auth; retry with `-u <user>` to confirm the backend beyond it.
