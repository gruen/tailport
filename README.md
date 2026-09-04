# tailport

A terminal UI that lists your machine's locally listening TCP ports and lets
you toggle `tailscale serve --http=<port>` on or off for each one — a quick
way to expose a local dev server to your [Tailscale](https://tailscale.com)
tailnet (your private WireGuard network) at `http://<hostname>:<port>`,
without touching a terminal or remembering `tailscale serve` syntax.

This is a personal tool built for a specific home tailnet setup (a handful
of Linux and macOS machines). It's shared as-is in case it's useful to
someone else, but it isn't a general-purpose product and makes no promises
about working outside that kind of setup.

## Security model

By default, tailport exposes ports only to your **tailnet** — the private
network of devices you've authenticated into Tailscale — by shelling out to
`tailscale serve` (plain HTTP, tailnet-only).

It can also expose a port to the **public internet**, but only as an explicit
opt-in: the `P` key funnels the selected port via `tailscale funnel`, behind
a strong y/n confirmation. Funnel is HTTPS-only and uses one of Tailscale's
three public ingress ports (443, 8443, 10000), so a funnelled port is
reachable by anyone on the internet — not just your tailnet. Public exposure
is never automatic; it happens only when you press `P` and confirm, `:22`
(SSH) is refused outright, and funnelled ports are drawn with a distinct
marker (`●` / 🐦).

Two deliberate constraints on the tailnet-`serve` path:

- **Plain HTTP** (`tailscale serve --http=<port>`), never HTTPS/TLS serve
  mode. Tailscale's WireGuard tunnel already encrypts traffic between tailnet
  peers, so app-layer TLS on top wouldn't add real confidentiality here — it
  would just add certificate handling for no benefit. (Funnel is necessarily
  HTTPS, since it faces the public internet.)
- **1:1 port mapping.** A tailnet-served port always keeps its own number
  (same port in and out); serve never remaps to a different number. Funnel is
  the deliberate exception — it maps your local port onto one of the public
  ingress ports (443/8443/10000), which won't match the local number.

## Requirements

- The [`tailscale`](https://tailscale.com/download) CLI installed,
  authenticated, and connected to a tailnet with
  [MagicDNS](https://tailscale.com/kb/1081/magicdns) enabled (so
  `http://<hostname>:<port>` resolves for your other tailnet devices).
- Run this once so tailport can call `tailscale serve` without root:
  ```sh
  sudo tailscale set --operator=$(whoami)
  ```
- Linux (uses `ss` for port discovery) or macOS (uses `lsof`). Other
  platforms aren't supported.
- Prebuilt release binaries are published for `linux/amd64`,
  `linux/arm64`, and `darwin/arm64` (see Install below). Other
  OS/architecture combinations require building from source with `go
  install`.

## Install

**On Arch Linux**, from the AUR. [`tailport`](https://aur.archlinux.org/packages/tailport)
builds from source; [`tailport-bin`](https://aur.archlinux.org/packages/tailport-bin)
drops in the prebuilt release binary and needs no Go toolchain. They conflict
with each other by design — install one:

```sh
paru -S tailport        # or: yay -S tailport
paru -S tailport-bin    # prebuilt
```

**On macOS or Linux**, from the [Homebrew](https://brew.sh) tap. It builds from
source, so it works on Apple Silicon and Intel Macs alike, and on Linuxbrew:

```sh
brew install gruen/tap/tailport
```

**With Go installed**, for any supported OS/arch:

```sh
go install github.com/gruen/tailport/cmd/tailport@latest
```

**Without Go**, on Linux (`amd64`/`arm64`) or macOS (`arm64`), fetch a
prebuilt binary from this repo's
[GitHub Releases](https://github.com/gruen/tailport/releases) using the
bundled install script. Either run it after cloning:

```sh
./install.sh
```

or fetch and run it directly:

```sh
curl -fsSL https://raw.githubusercontent.com/gruen/tailport/main/install.sh | sh
```

The script detects your OS and architecture, downloads the matching binary
from the latest release, verifies it against the release's published
`sha256` checksum, and installs it to `~/.local/bin/tailport`. Override the
destination directory with `TAILPORT_INSTALL_DIR`, or pin a specific release
with `TAILPORT_VERSION` (e.g. `TAILPORT_VERSION=0.1.1`; a leading `v` is
accepted too) instead of taking the latest. Release binaries are built by
`.github/workflows/build.yml` on tagged pushes (`v*`); if that workflow's
build matrix doesn't cover your platform, use `go install` instead.

Re-running the script is version-aware and safe to script into a cron job or
dotfiles bootstrap:

- If the version already installed matches the target, it prints
  `already up to date` and does nothing.
- If the upgrade (or downgrade) isn't breaking, it backs up the previous
  binary to `tailport.bak` next to the install, installs the new one, and
  prints the old → new version.
- If the transition **is** breaking — a **major** version change (a `0.x`
  minor bump is *not* breaking; it upgrades like any other) — the script
  refuses to install and exits non-zero, leaving the existing binary
  untouched. Review the release notes, then opt in explicitly with
  `TAILPORT_ALLOW_BREAKING=1` to install anyway (this also backs up the old
  binary first). This gate is skipped, with a note, only when the
  currently-installed binary's version can't be determined (e.g. it predates
  `--version` support).

A rolling backup (`~/.local/bin/tailport.bak`, or `$TAILPORT_INSTALL_DIR/tailport.bak`)
is kept whenever the script replaces an existing install; roll back with
`mv ~/.local/bin/tailport.bak ~/.local/bin/tailport`.

The default install directory, `~/.local/bin`, isn't on every system's
`PATH`. If the `tailport` command isn't found after installing, add it to
your shell's rc file (`~/.bashrc`, `~/.zshrc`, …):

```sh
export PATH="$HOME/.local/bin:$PATH"
```

## Usage

Run `tailport`. It scans locally listening TCP ports and shows how each one
is actually reachable — localhost only, already on your tailnet, or served
(and to whom).

| Key | Action |
| --- | --- |
| `↑`/`↓`, `j`/`k` | Move the selection between **route** sub-rows — a flattened walk across every service's active routes |
| `Shift+↑`/`Shift+↓`, `J`/`K` | Jump the selection between **services**, landing on the target service's first route |
| `space` | Toggle `tailscale serve` (tailnet-only) on/off for the selected port — only offered for a loopback-bound port; an already-reachable (tailnet/LAN) port shows an info toast instead |
| `P` | Funnel the selected port to the **public internet** via `tailscale funnel`, behind a strong y/n confirm (`:22` refused). Press again to drop it back to tailnet-served |
| `p` | Publish the selected port to the **public internet** via a Caddy edge (a custom `https://` hostname, no port in the URL), behind a strong y/n confirm (`:22` refused). Press again to unpublish |
| `e` | Change a published port's hostname or auth without unpublishing it first — runs the same setup flow as `p`, ending in the same confirm |
| `t` | Tunnel the selected port to the **public internet** via a Cloudflare Tunnel (`cloudflared`), quick (random `*.trycloudflare.com`) or named (your routed hostname). Press again to tear it down. Only offered when `cloudflared` is installed; `:22` refused |
| `c` / `y` | Copy the **selected route's** exact URL to the clipboard (via OSC 52, so it works over SSH) — `y` is a yank alias for `c`. Every other action stays service-scoped; only copy acts on the specific route sub-row you've navigated to. The copy is confirmed inline with a ✓ on that route's line, or by a toast (e.g. an `offline` route or a still-starting quick tunnel has no URL yet) |
| `C` | Tear down stale forwards — ports still served with nothing listening locally. Offered only when some exist |
| `x` | Lock / unlock the selected port. A locked port can't be served until unlocked; `:22` is locked by default and unlocking it requires typing `ssh` |
| `n` | Add a port by number to Favorites (even one nothing is listening on yet). It does **not** serve — press `space` there to serve it once its service is up |
| `l` | Label the selected port with custom text (prefilled with its current label if set, else the process name) |
| `f` | Favorite the selected port, pinning it to the default view |
| `F` | Forget the selected port: clears its ★ and drops it from the default view |
| `u` | Undo the last registry edit (favorite, forget, label, lock, add) — session-only, and never touches what's exposed |
| `ctrl+r` | Redo the last undone registry edit |
| `a` | Toggle between the default view and showing every listening port |
| `/` | Filter by port number, process, or label (fuzzy) |
| `r` | Refresh the port list and serve status |
| `?` | Toggle the full help overlay |
| `q` / `ctrl+c` | Quit |

Each service is shown as a small record: a header line (port number, name,
★/🔒 badges) followed by one **route** sub-row for every way it's actually
reachable right now — `localhost` (loopback), `LAN` (bound to a specific LAN
IP), `tailnet` (already reachable, or served via `tailscale serve`), `ts.net`
(funnelled), `caddy` (published), and `cloudflare` (tunnelled). A service can
show several routes at once, and — since the three public paths are
independent of each other — that includes multiple *public* routes
simultaneously: a port can be funnelled, published, and tunnelled all at the
same time, each its own row with its own marker and its own exact URL. A down
favorite (nothing listening, nothing served) shows a single `offline`
pseudo-route instead. Every route has its own leading marker encoding its
type; see [Status markers](#status-markers) below for the exact glyphs. A
favorited service additionally shows a star (★) on its header. The name shown
next to a port is its custom label if you've set one, otherwise its resolved
process name (or `was <name>` for a favorite whose process has since exited)
— or `?` if that can't be determined, which happens when the port belongs to
a process owned by a different user (most commonly `root`) than the one
running tailport.

### Default view and the port registry

tailport doesn't show every listening port by default — that gets noisy
fast (sshd, mDNS, Docker, browsers holding sockets open, etc.). Instead it
shows the union of:

- ports currently served via `tailscale serve`, and
- ports in the **registry**: anything you've ever toggled on, labeled, or
  favorited.

A port earns a place in the registry the moment you interact with it —
serving it (`space`), adding it by number (`n`), labeling it (`l`),
favoriting it (`f`), or locking it (`x`) all add it. Once a port is in the
registry it keeps showing up, marked inactive, even after you toggle it off —
and that persists across restarts, not just for the current session. `F`
(forget) on a port that has no label and isn't locked reverses this: it's
dropped from the registry and disappears from the default view (unless it's
currently active).

Registry edits are undoable within a session: `u` steps back through them one
at a time and `ctrl+r` steps forward. Undo covers only the registry —
favorites, labels, locks, adds. It never changes what's actually exposed:
serve and funnel have their own keys and confirms, and undo won't flip them
behind your back. For the same reason it won't unlock `:22`, since that needs
a deliberate typed confirm.

Press `a` to bypass the registry entirely and see every port currently
listening on the machine, whether known to tailport or not — useful for
finding something new to serve, label, or favorite.

## Configuration

On first run, tailport writes a registry seeded with `:22` (SSH) locked to:

```
$XDG_CONFIG_HOME/tailport/config.yaml
```

or, if `XDG_CONFIG_HOME` isn't set, `~/.config/tailport/config.yaml`. It
won't overwrite an existing file. This is the port registry described
above — labels, favorites, and locks, keyed by port number — and it's
rewritten automatically every time you toggle, label, favorite/unfavorite, or
lock a port from within the app. You generally shouldn't need to hand-edit
it, but the format is plain YAML if you want to:

```yaml
ports:
    22:
        locked: true
    3000:
        label: dev server
        favorite: true
    9000: {}
```

An entry can have a `label`, be marked `favorite`, and/or be `locked` (a
locked port can't be served until you unlock it — `:22` ships locked by
default). An empty entry (`{}`, as for `9000` above) means "keep this in the
default view" without any of those — the state left behind by serving a port
without labeling or favoriting it. tailport also records a `last_process` key
per port automatically (the name it last saw listening, used for the
`was <name>` display); you don't set that by hand.

### Status markers

A top-level `markers` key (or the equivalent `--markers` flag, which wins
over the config value for that run only) selects how each route sub-row's
marker is drawn:

```yaml
markers: "" # "" / mono (default) | auto | emoji | ascii
```

- unset (`""`, the default) — mono: ○ localhost · ◔ local network ·
  ◑ on tailnet · ◉ served · ● public (funnel) · ◆ public (published) ·
  ◈ public (cloudflare tunnel) · ▲ stale (dangling forward) · ✕ offline.
- `auto` — opts into detecting a UTF-8-capable terminal (locale is UTF-8 and
  `TERM` isn't the bare Linux console or `dumb`) and switches to the
  moon-phase emoji ramp there, otherwise falls back to mono: 🌕 localhost ·
  🌔 local network · 🌓 on tailnet · 🌒 served · 🌑 public (funnel) ·
  🌐 public (published) · ☁️ public (cloudflare tunnel) · 🌫️ stale ·
  ✕ offline.
- `emoji` — always the moon-phase ramp above, regardless of terminal.
- `ascii` — always mono, regardless of terminal (same glyphs as unset).

This setting governs the exposure markers only. Any other emoji/animation
tailport might render (e.g. from its hidden Easter-egg overlay) always
auto-detects terminal capability on its own, independent of `markers`.

### Theme (light/dark terminals)

tailport auto-detects your terminal's background and picks legible colors
either way. If detection guesses wrong (common over SSH/tmux/some
multiplexers), override it with a top-level `theme` key:

```yaml
theme: auto # auto (default) | light | dark
```

or the equivalent `--theme` flag (`--theme light`, `--theme dark`,
`--theme auto`), which wins over the config value. `auto` detects the
background itself; when it can't tell at all, it falls back to `dark` --
existing dark-terminal setups see no change either way.

### Publish (Caddy edge)

A `caddy` block configures the optional publish-to-the-internet path (see
[Publishing to the public internet](#publishing-to-the-public-internet-caddy-edge)
below). Unlike the port registry, tailport writes this block in full — with
visible defaults and explanatory comments — the first time it saves the config
once the feature is present, so the available knobs are discoverable without
reading docs:

> **Upgraded from an older tailport?** A `config.yaml` written before this
> feature landed has **no `caddy:` block yet** — that's expected, and it's why
> you won't find a `domain:` line to edit. It appears on the next save — any
> change that writes the file, e.g. favouriting or labelling a port — or just
> paste the block below in by hand and set `domain:` there. (Publishing can
> also be the trigger: pressing `p` with a blank `domain` captures it inline
> and saves it for you, rather than refusing.)

```yaml
caddy:
    # Tailnet name of the Caddy edge node; tailport reaches its admin API
    # here. Use the short MagicDNS label, not an FQDN.
    hostname: caddy

    # Public base domain used to build publish hostnames. Point its DNS
    # (typically a wildcard) at the public Caddy edge before publishing.
    domain: ""

    # Name of the shared Caddy JSON HTTP server under apps.http.servers.
    # Every tailport computer publishing through this same Caddy edge must
    # use the same value; this does not identify the source computer.
    server_name: tailport

    # Port of the Caddy admin API on the edge (reachable tailnet-only).
    admin_port: 2019

    # Skip the y/n confirm when re-publishing a port already published
    # earlier this session (remembered hostname + auth). First publish
    # always confirms. Default false (confirm shown).
    silent_republish: false
```

- **`hostname`** (default `caddy`) — the edge's own private tailnet
  identity, used only so tailport can find its admin API at
  `http://<hostname>:<admin_port>`. Use the short MagicDNS label, **not** an
  FQDN — the edge admits only its short name, so an FQDN silently 403s and
  publishing rejects one. It has nothing to do with any published route's
  public hostname (e.g. `app.example.com`) — private edge identity and public
  route identity are deliberately separate.
- **`domain`** (default `""`, blank) — the public base domain publish
  hostnames are built from. Blank doesn't block publishing: pressing `p`
  captures the domain inline and saves it before continuing (rather than
  refusing), and until it's set the background published-state poll doesn't
  run (zero cost until you set it).
- **`server_name`** (default `tailport`) — the shared Caddy HTTP server
  tailport manages. Every tailport computer publishing through the same
  edge must agree on this value; it selects the routes array, it does not
  identify the source computer.
- **`admin_port`** (default `2019`) — the Caddy admin API's port on the
  edge.
- **`auth_user`** / **`auth_hash`** — unset (no auth) until you opt into
  basic auth at a publish confirmation. `auth_hash` is always a bcrypt hash
  of the password you typed then, never the plaintext; every published
  route that opts into auth shares this one credential — it isn't
  per-hostname.
- **`silent_republish`** (default `false`) — skips the `p` key's y/n confirm
  when RE-publishing a port that was already published earlier in the SAME
  session (its hostname and auth are remembered in memory only; see
  [Publish is a toggle](#publish-is-a-toggle-p) below). A port's first
  publish this session always confirms regardless of this setting, and
  Funnel's confirm is unaffected. Off by default: the confirm is shown
  unless you explicitly opt in.

None of this configures the edge itself — it only tells tailport where an
**already-deployed** edge lives. Standing up the edge (on Fly.io or any host
you run — Tailscale ACL and auth key, DNS) is a separate one-time operator
task; see [`docs/caddy-edge.md`](docs/caddy-edge.md).

### Tunnel (Cloudflare)

A `cloudflared` block configures the optional tunnel-to-the-internet path
(see [Tunnelling to the public internet](#tunnelling-to-the-public-internet-cloudflare-tunnel)
below). Like the `caddy` block, tailport writes this block in full — with
visible defaults and explanatory comments — the first time it saves the
config once the feature is present, so both knobs are discoverable without
reading docs.

> **Upgraded from an older tailport?** A `config.yaml` written before this
> feature landed has **no `cloudflared:` block yet** — that's expected. It
> appears on the next save — any change that writes the file, e.g.
> favouriting or labelling a port — or just paste the block below in by hand.
> Unlike `caddy.domain`, `cloudflared.domain` gates nothing: it's a pure
> convenience prefill, so there's no equivalent of publish's "captures it
> inline on first use" behavior here.

```yaml
cloudflared:
    # Optional path to the cloudflared executable. Blank means tailport
    # looks up `cloudflared` on $PATH.
    binary: ""

    # Optional public base domain used to prefill the hostname prompt when
    # starting a named (authenticated) tunnel. Blank by default; quick
    # (unauthenticated) tunnels ignore it.
    domain: ""
```

- **`binary`** (default `""`, blank) — path to the cloudflared executable.
  Blank means tailport looks up `cloudflared` on `$PATH`; the whole `t`
  feature (key, discovery, polling) stays dormant unless it's found there
  (or at this path).
- **`domain`** (default `""`, blank) — a public base domain used only to
  **prefill** the hostname prompt when starting a **named** tunnel (see
  [The tunnel toggle](#the-tunnel-toggle-t) below). Purely a convenience:
  leaving it blank doesn't block anything, and a **quick** tunnel ignores it
  entirely — its hostname is assigned by Cloudflare, not built from this
  domain.

None of this configures Cloudflare itself — for a named tunnel, logging in
(`cloudflared tunnel login`), creating the tunnel, and routing its hostname
(`cloudflared tunnel route dns`) are a separate, one-time operator task;
tailport only ever *runs* an already-provisioned named tunnel. See
[Tunnelling to the public internet](#tunnelling-to-the-public-internet-cloudflare-tunnel)
below.

## How it works

- Port discovery: `ss -H -t -l -n -p` on Linux, `lsof -iTCP -sTCP:LISTEN -n
  -P` on macOS, run locally — tailport never scans the network.
- Serve status: `tailscale serve status --json`, parsed to find which ports
  currently have an active HTTP mapping.
- Toggling on: `tailscale serve --bg --http=<port> <port>`.
- Toggling off: `tailscale serve --http=<port> off` (a surgical removal of
  just that one mapping; other active mappings are left alone).
- Registry writes: the config file is rewritten immediately after every
  toggle, label, or favorite/unfavorite — there's no in-memory-only state
  to lose if tailport is killed rather than quit normally.

tailport has no dependencies beyond the `tailscale` CLI and the OS tools
above — no daemon, no config beyond the YAML file, and nothing is installed
or modified system-wide other than the `serve` mappings you toggle
yourself.

## Publishing to the public internet (Caddy edge)

Tailnet `serve` and Funnel aren't the only way out to the world: tailport can
also publish a port to a **custom public hostname** —
`https://app.example.com`, no port in the URL, no `*.ts.net` — through a
Caddy edge node you run yourself (on Fly.io, or any host that meets the
requirements; see below). This is a third
exposure level, architecturally independent of both `serve` (tailnet) and
Funnel: Tailscale's role in this path is private WireGuard transport from
the edge to your machine only, and nothing more — Caddy owns the entire
public trust plane (custom-domain DNS, `:443` ingress, TLS termination and
certificate issuance/renewal, hostname routing). No Funnel slots, Funnel
commands, or Tailscale-managed public TLS participate in a publish.

Publishing and Funnel are **independent, not ranked** — and, since kata th05,
no longer mutually exclusive: a port can be funnelled AND published at the
same time, each showing as its own route sub-row with its own marker and
exact URL. There is no "publish outranks funnel" — normal use never needs to
rank them — and each still requires its own strong per-service confirm before
going live; `:22` stays hard-blocked from both.

**Setup is a separate, one-time operator task**, not something tailport
does for you: a Caddy edge deployed and reachable on your tailnet, a
domain whose DNS points at it, and a Tailscale auth key for the edge itself.
See [`docs/caddy-edge.md`](docs/caddy-edge.md) for the full runbook (written
for a Caddy/Fly first-timer) and the `caddy.*` fields under
[Configuration](#configuration) above for what tailport needs once that edge
exists. Until `caddy.domain` is set, the background published-state poll
stays off; the first time you press `p`, tailport captures the domain inline
and saves it before continuing, so you don't have to edit the config by hand.

Once configured, publishing a port works the same shape as Funnel: select a
port, confirm the public hostname and (optionally) a shared basic-auth
credential, and confirm again against the exact `https://` URL before
anything goes live — the same funnel-grade guardrails (`:22` hard-blocked,
public exposure never automatic) apply here too.

### Publish is a toggle (`p`)

`p` behaves differently depending on the port's state:

- **Already published** — `p` unpublishes immediately. No confirm: reducing
  exposure is never gated.
- **Published earlier this SESSION, then unpublished** — tailport remembers
  that port's hostname and auth in memory (never written to config; the edge
  stays the source of truth) for as long as the process runs. Pressing `p`
  again re-publishes with that remembered config, skipping the hostname/auth
  setup prompts entirely — straight to the same y/n confirm naming the exact
  `https://<hostname>`, unless you've set `silent_republish: true` (see
  [Configuration](#configuration) above), in which case it re-publishes with
  no confirm at all.
- **Never published this session** — `p` runs the full setup: hostname,
  optional basic auth, then the confirm. This always happens on a port's
  first publish, regardless of `silent_republish`.

Press **`e`** to change a published port's hostname or auth **without**
unpublishing it first: `e` always runs the full setup flow (prefilled with
the port's current/remembered hostname when known), ending in the same y/n
confirm `p` uses. Confirming replaces the live route with the new
hostname/auth in place — the port is never briefly unpublished in between.

## Tunnelling to the public internet (Cloudflare Tunnel)

Tailnet `serve`, Funnel, and Publish aren't the only way out to the world:
tailport can also tunnel a port to the public internet through a
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/install-and-setup/installation/),
run by the `cloudflared` CLI. This is a **third** exposure path, sibling to
Funnel and Publish rather than layered above or below either — and
architecturally different from both: cloudflared is a **long-running local
process** tailport supervises directly, not a remote edge tailport pokes (as
with Publish) or a Tailscale-managed public ingress slot (as with Funnel).
The `cloudflared` binary *is* the connector; a tunnel is up only while its
process stays alive.

The whole feature exists only when `cloudflared` is actually installed:
tailport detects it once at startup, and when it's absent the `t` key is
dropped from the bar entirely — no key, no discovery, no polling, zero cost.
There are two flavors, matching Cloudflare's two account scenarios:

- **Quick tunnel** — no Cloudflare account needed. tailport runs
  `cloudflared tunnel --url http://localhost:<port>`, which hands back a
  random `https://<name>.trycloudflare.com` hostname: unauthenticated, and
  ephemeral — a new hostname every time you start one.
- **Named tunnel** — for an authenticated account. You've already run
  `cloudflared tunnel login`, created a tunnel, and routed a stable custom
  hostname to it (`cloudflared tunnel route dns`) — a separate, one-time
  operator task, exactly like standing up the Caddy edge, that tailport does
  not do for you and never automates. tailport only *runs* that
  pre-provisioned tunnel — `cloudflared tunnel run --url
  http://localhost:<port> <name>` — bound to your stable hostname. It never
  mutates your Cloudflare account or DNS.

**Tunnels survive tailport exiting.** cloudflared is started detached, in its
own session, so quitting the TUI doesn't drop the tunnel — it keeps running
until you tear it down or kill it yourself. tailport never persists tunnel
state to disk; instead it reads the OS process table live on every poll, so a
tunnel started in a previous tailport session is re-discovered the next time
you launch it and stays re-toggleable with `t`. Only **tailport-owned**
tunnels — the ones carrying a sentinel `--logfile` flag tailport always
passes — are tracked this way; a `cloudflared` process started outside
tailport is left alone entirely, never signalled or touched.

Tunnelling, Funnel, and Publish are **independent, not ranked** — and, since
kata th05, no longer mutually exclusive: a port can carry all three public
exposures at once, each showing as its own route sub-row. There is no
implicit precedence between any of the three, and multiple public routes on
one port is a legitimate, expected state, not something tailport flags or
collapses. Every public path still requires its own strong per-service
confirm before going live, and `:22` stays hard-blocked from all three.

A tunnelled service shows its own `cloudflare` route sub-row with a distinct
marker (`◈` / ☁️) — see [Status markers](#status-markers) above — with the
exact public URL once known, alongside any other active routes for that same
service.

### The tunnel toggle (`t`)

`t` behaves differently depending on the port's state:

- **Already tunnelled** — `t` tears the tunnel down immediately. No confirm:
  reducing exposure is never gated.
- **Tunnelled earlier this SESSION, then torn down** — tailport remembers
  that port's mode (and, for a named tunnel, its hostname) in memory only,
  for as long as the process runs. Pressing `t` again re-raises it, skipping
  the setup prompts entirely — straight to the same confirm.
- **Never tunnelled this session** — `t` runs the full setup. If you're
  logged in to Cloudflare (a credential from `cloudflared tunnel login` is
  present), you're asked to pick quick or named; choosing named then asks for
  the hostname you've already routed and the tunnel's name. Without an
  account, only the quick path exists, so setup skips straight to its
  confirm.

Every path ends in a y/n confirm before anything goes live, and `:22` (SSH)
is hard-blocked — the same funnel/publish-grade guardrails. The **quick**
tunnel's confirm is a deliberate exception to the "always name the exact
public URL" rule: cloudflared assigns the `*.trycloudflare.com` hostname only
after the tunnel actually starts, so there is no URL to name in advance. The
confirm instead names the local port; tailport flashes `starting Cloudflare
quick tunnel for :<port>…`, and the real `https://…` address appears in the
row's description a few seconds later, once the next poll picks it up. A
**named** tunnel's confirm has no such gap — it names the exact
`https://<hostname>` up front, the same as Publish.

## Troubleshooting

### Dangling forward (`▲` / `🪹`, "bound to tailnet, but stale")

A row marked `▲` / `🪹` — whose description reads *"bound to tailnet, but
stale — space to unbind"* — means the `serve` mapping is up but no local
process holds the port. Two common cases:

- **The app just isn't running** (it died, or hasn't started). Start it, or
  unbind the port — `space` on the row, or `C` to clear all stale forwards.
  The mapping deliberately outlives the app so you can restart it freely, so
  tailport won't tear it down for you.
- **The app can't start with "address already in use."** When you serve
  `:8025`, tailscaled binds your **tailnet IP** on `:8025`. If your app then
  tries to bind `0.0.0.0:8025` (all interfaces), that collides and the app
  fails to start — so the forward dangles. The mapping meant to serve the app
  is what's blocking it.

  The fix is to bind the app to **loopback**, which is what `serve` proxies to
  anyway:

  ```sh
  mailpit --listen 127.0.0.1:8025      # e.g. — bind 127.0.0.1, not 0.0.0.0
  ```

  This both resolves the collision and keeps the app off your LAN — it's
  reachable only over the tailnet, through `serve`. If you genuinely need the
  app on `0.0.0.0:<port>`, unbind the port first (`space`, or `C`) — note
  that once it's bound to `0.0.0.0`, it's already reachable on the tailnet
  on its own (state `on tailnet`), so there's no longer anything to serve.

## Development

Build and test locally with the standard Go toolchain:

```sh
go build ./...
go vet ./...
go test ./...
```

### CI and the macOS `lsof` path

Port discovery is OS-specific: Linux uses `ss`, macOS uses `lsof` (see
[How it works](#how-it-works)). The default CI runs on Linux, so the macOS
`lsof` code path in `internal/portscan` is built when cross-compiling but is
**not executed** there.

To keep pricey macOS runner minutes opt-in, the macOS-specific tests run on a
native Apple-Silicon runner only when you ask for them, via
[`darwin-tests.yml`](.github/workflows/darwin-tests.yml):

- **Include `[ci darwin]` in a commit message** and push — the macOS job runs
  `go build/vet/test` on `macos-14`, so the darwin-tagged tests in
  `internal/portscan` (the `parseLsof` fixtures and the real-`lsof` `List()`
  smoke test) actually execute.
- Or trigger it manually from the repository's **Actions** tab
  (`workflow_dispatch`).

A push **without** the `[ci darwin]` token does not start the macOS job. The
token is read from the pushed commit message, so use a branch push or manual
dispatch (it is not evaluated for pull-request events).

## License

[MIT](./LICENSE)
