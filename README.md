# tailport

A terminal UI for exposing your machine's local dev servers across your
[Tailscale](https://tailscale.com) tailnet. It lists every locally listening
TCP port and lets you flip `tailscale serve` on or off for each one with a
keypress — so a server on `localhost:3000` becomes reachable at
`http://<hostname>:3000` from your other tailnet devices, without memorizing
`tailscale serve` syntax.

By default nothing leaves your tailnet — your private WireGuard network. When
you want it to, tailport can also expose a port to the **public internet**, but
only as a deliberate, per-port, confirmed opt-in — see
[Exposing a port](#exposing-a-port).

> This is a personal tool built for one specific home tailnet — a handful of
> Linux and macOS machines. It's shared as-is in case it's useful to someone
> else; it isn't a general-purpose product and makes no promises about working
> outside that kind of setup.

## Quickstart

1. **Install it** ([all options below](#install)):
   ```sh
   brew install gruen/tap/tailport
   ```
2. **Let tailport call `tailscale serve` without root** — a one-time grant:
   ```sh
   sudo tailscale set --operator=$(whoami)
   ```
   You'll also need the `tailscale` CLI installed, authenticated, and on a
   tailnet with [MagicDNS](https://tailscale.com/kb/1081/magicdns) enabled (so
   `http://<hostname>:<port>` resolves for your other devices) — see
   [Requirements](#requirements).
3. **Run it:**
   ```sh
   tailport
   ```
   Arrow-key to a port, press `t` to serve it on your tailnet, then `c` to
   copy its URL. Press `?` for the full keybinding overlay, or run
   `tailport quickstart` for a non-interactive tour.

## Requirements

- The [`tailscale`](https://tailscale.com/download) CLI installed,
  authenticated, and connected to a tailnet with
  [MagicDNS](https://tailscale.com/kb/1081/magicdns) enabled.
- The one-time operator grant above (`sudo tailscale set --operator=$(whoami)`),
  so tailport can toggle `serve` without root.
- Linux (uses `ss` for port discovery) or macOS (uses `lsof`). Other platforms
  aren't supported.
- Prebuilt release binaries are published for `linux/amd64`, `linux/arm64`,
  and `darwin/arm64`. Other OS/architecture combinations require building from
  source with `go install`.

## Install

**On Arch Linux**, from the AUR.
[`tailport`](https://aur.archlinux.org/packages/tailport) builds from source;
[`tailport-bin`](https://aur.archlinux.org/packages/tailport-bin) drops in the
prebuilt release binary and needs no Go toolchain. They conflict with each
other by design — install one:

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

**Without Go**, on Linux (`amd64`/`arm64`) or macOS (`arm64`), fetch a prebuilt
binary from this repo's
[GitHub Releases](https://github.com/gruen/tailport/releases) with the bundled
install script — either after cloning:

```sh
./install.sh
```

or directly:

```sh
curl -fsSL https://raw.githubusercontent.com/gruen/tailport/main/install.sh | sh
```

The script detects your OS and architecture, downloads the matching binary from
the latest release, verifies it against the release's published `sha256`
checksum, and installs it to `~/.local/bin/tailport`. Override the destination
with `TAILPORT_INSTALL_DIR`, or pin a release with `TAILPORT_VERSION` (e.g.
`TAILPORT_VERSION=0.1.1`; a leading `v` is accepted) instead of taking the
latest.

Re-running the script is version-aware and safe to script into a cron job or
dotfiles bootstrap:

- If the installed version already matches the target, it prints
  `already up to date` and does nothing.
- If the upgrade (or downgrade) isn't breaking, it backs up the previous binary
  to `tailport.bak` next to the install, installs the new one, and prints the
  old → new version.
- If the transition **is** breaking — a **major** version change (a `0.x` minor
  bump is *not* breaking) — the script refuses and exits non-zero, leaving the
  existing binary untouched. Review the release notes, then opt in with
  `TAILPORT_ALLOW_BREAKING=1` to install anyway (it still backs up first). This
  gate is skipped, with a note, only when the installed binary's version can't
  be determined (e.g. it predates `--version` support).

A rolling backup (`tailport.bak` next to the install) is kept whenever the
script replaces an existing binary; roll back with
`mv ~/.local/bin/tailport.bak ~/.local/bin/tailport`.

> `~/.local/bin` isn't on every system's `PATH`. If `tailport` isn't found after
> installing, add it to your shell's rc file:
> `export PATH="$HOME/.local/bin:$PATH"`.

Once installed by binary or script, `tailport update` self-updates in place (see
[Command-line reference](#command-line-reference)). Homebrew and AUR installs
update through their package manager instead.

## What you see

Run `tailport`. It scans locally listening TCP ports — it never scans the
network — and shows each service as a small record: a header line (port number,
name, ★/🔒 badges) followed by one **route** sub-row for every way that service
is reachable right now:

| Route | Meaning |
| --- | --- |
| `localhost` | Loopback only — not exposed |
| `LAN` | Bound to a specific LAN IP |
| `tailnet` | Reachable on your tailnet (already, or via `tailscale serve`) |
| `ts.net` | Funnelled to the public internet |
| `caddy` | Published to a custom public hostname |
| `cloudflare` | Tunnelled to the public internet |

A service can show several routes at once — including multiple *public* routes,
since the three public paths are independent: a port can be funnelled,
published, and tunnelled at the same time, each its own row with its own marker
and exact URL. A down favorite (nothing listening, nothing served) shows a
single `offline` row instead. Every route carries a leading marker glyph
encoding its type — see [Status markers](#status-markers). A favorited service
shows a ★ on its header.

The name next to a port is your custom label if you've set one, otherwise the
resolved process name — or `was <name>` for a favorite whose process has exited,
or `?` when the port belongs to a process owned by a different user (commonly
`root`) than the one running tailport.

### Keybindings

| Key | Action |
| --- | --- |
| `↑`/`↓`, `j`/`k` | Move between route rows (a flat walk across every service's routes) |
| `Shift+↑`/`Shift+↓`, `J`/`K` | Jump between services (land on the target's first route) |
| `t` | Serve on your tailnet — toggle (loopback-bound ports only) |
| `P` | Funnel to the public internet — toggle, behind a confirm |
| `p` | Publish via your Caddy edge — toggle, behind a confirm |
| `e` | Edit a published port's auth in place (can't move it to a new hostname) |
| `o` | Cloudflare tunnel — toggle, behind a confirm (only when `cloudflared` is installed) |
| `c` / `y` | Copy the selected **route's** URL to the clipboard (via OSC 52, so it works over SSH) |
| `i` | Copy the selected **port's** bare PID (e.g. `12345`) — refuses if it can't be resolved |
| `I` | Copy a ready-to-run `kill <pid>` command (SIGTERM) — same refusal as `i` |
| `C` | Tear down stale forwards (served with nothing listening) |
| `x` | Lock / unlock the selected port (`:22` ships locked; unlocking it needs a typed `ssh`) |
| `n` | Add a port to Favorites by number (even one nothing's listening on yet) |
| `l` | Label the selected port |
| `f` | Favorite the selected port (pin to the default view) |
| `F` | Forget the selected port (clear ★, drop from the default view) |
| `u` | Undo the last registry edit (favorite/forget/label/lock/add) |
| `ctrl+r` | Redo the last undone registry edit |
| `a` | Show every listening port — toggle |
| `/` | Filter by port number, process, or label (fuzzy) |
| `r` | Refresh the port list and serve status |
| `h` | Show/hide the bottom-bar keybinding legend |
| `?` | Toggle the full help overlay |
| `q` / `ctrl+c` | Quit |

Every action is service-scoped **except** copy (`c`/`y`), which acts on the
exact route row you've navigated to. A copy is confirmed inline with a ✓ on that
route's line, or by a toast when the route has no URL yet (e.g. an `offline`
route or a still-starting quick tunnel).

`i` and `I` copy a property of the **port**, not the route, so they resolve to
the port even when a route sub-row is selected, and always confirm by toast
(there's no per-route line to annotate). Both refuse — a toast naming the
port, nothing copied — when the port's PID can't be resolved (`0`): a
foreign-owned port, or a favorite that's currently down.

## Exposing a port

tailport has four exposure levels, in increasing reach. Every one is **opt-in
and per-port** — tailport never exposes anything on its own — and `:22` (SSH) is
hard-blocked from all three public paths.

| Level | Key | Reach | Transport |
| --- | --- | --- | --- |
| **Serve** | `t` | Your tailnet | `tailscale serve` (plain HTTP) |
| **Funnel** | `P` | Public internet | `tailscale funnel` (HTTPS via `*.ts.net`) |
| **Publish** | `p` | Public internet | Your own [Caddy edge](#publishing-to-the-public-internet-caddy-edge) — custom `https://` hostname |
| **Tunnel** | `o` | Public internet | [Cloudflare Tunnel](#tunnelling-to-the-public-internet-cloudflare-tunnel) (`cloudflared`) |

**Serve** is the default path and the reason tailport exists. Press `t` on a
loopback-bound port and it's reachable at `http://<hostname>:<port>` across your
tailnet. (An already-reachable port shows an info toast instead — there's
nothing to serve.) Two deliberate constraints:

- **Plain HTTP**, never HTTPS/TLS serve mode. Tailscale's WireGuard tunnel
  already encrypts traffic between tailnet peers, so app-layer TLS would just
  add certificate handling for no real confidentiality here.
- **1:1 port mapping.** A served port always keeps its own number; serve never
  remaps.

**The three public paths** (`P`, `p`, `o`) each expose a port to *anyone on the
internet*, so each:

- requires a strong y/n confirmation before going live, naming the resulting
  URL (the one exception is a Cloudflare *quick* tunnel, whose random
  `*.trycloudflare.com` hostname isn't known until it starts);
- de-escalates instantly, with no confirm, when you press the same key again —
  reducing exposure is never gated;
- is independent of the others and may coexist on the same port; and
- refuses `:22` outright.

Funnel is the one public path that maps onto a fixed public ingress port
(Tailscale allows only 443/8443/10000), so a funnelled port's public number
won't match its local one.

See the runbooks below for setup and per-key behavior:
[Publish](#publishing-to-the-public-internet-caddy-edge) and
[Tunnel](#tunnelling-to-the-public-internet-cloudflare-tunnel).

## The default view and the port registry

tailport doesn't show every listening port by default — that gets noisy fast
(sshd, mDNS, Docker, browsers holding sockets open). Instead it shows the union
of:

- ports currently served via `tailscale serve`, and
- ports in the **registry**: anything you've ever toggled on, labeled,
  favorited, locked, or added by number.

A port earns its place in the registry the moment you interact with it — serving
(`t`), adding (`n`), labeling (`l`), favoriting (`f`), or locking (`x`) all
add it — and it keeps showing up (marked inactive) even after you toggle it off,
persisting across restarts. `F` (forget) on a port with no label and no lock
reverses this: it's dropped from the registry and disappears from the default
view (unless it's currently active).

Registry edits are undoable within a session: `u` steps back one at a time,
`ctrl+r` forward. Undo covers **only** the registry — favorites, labels, locks,
adds — and never changes what's actually exposed: serve and the public paths
have their own keys and confirms, and undo won't flip them behind your back. For
the same reason it won't unlock `:22`, which needs a deliberate typed confirm.

Press `a` to bypass the registry and see every port currently listening —
useful for finding something new to serve, label, or favorite.

## Command-line reference

`tailport` with no arguments launches the TUI. It also accepts:

**Flags** (a flag value wins over the config file for that run):

| Flag | Meaning |
| --- | --- |
| `-v`, `--version` | Print version and exit |
| `-c`, `--config <path>` | Use a specific config file (default resolves under `$XDG_CONFIG_HOME`, else `~/.config`) |
| `--no-color` | Disable ANSI color output (also honors `NO_COLOR`) |
| `--markers <mode>` | Exposure-glyph style: `auto`, `emoji`, or `ascii` — see [Status markers](#status-markers) |
| `--theme <mode>` | Color scheme: `auto`, `light`, or `dark` — see [Theme](#theme-lightdark-terminals) |

**Subcommands:**

- **`tailport quickstart`** — non-interactive onboarding and the keybinding
  legend, printed to stdout. A first look, or a cheat sheet, without entering
  the TUI.
- **`tailport status`** — a headless, read-only report of how each port is
  currently exposed. Add `--json` for machine-readable output. Changes nothing.
- **`tailport update`** — self-update to the latest release (sha256-verified).
  `--check` reports whether an update is available without installing it;
  `-y`/`--yes` skips the confirmation prompt; `--force` overrides its refusal to
  touch a package-manager-managed install. (Installed via Homebrew or the AUR?
  Update through that instead.)

## Configuration

On first run, tailport writes a registry seeded with `:22` (SSH) locked to:

```
$XDG_CONFIG_HOME/tailport/config.yaml
```

or, if `XDG_CONFIG_HOME` isn't set, `~/.config/tailport/config.yaml`. It won't
overwrite an existing file. This is the port registry described
[above](#the-default-view-and-the-port-registry) — labels, favorites, and locks
keyed by port number — and it's rewritten automatically whenever you toggle,
label, favorite/unfavorite, or lock a port from within the app. You generally
shouldn't need to hand-edit it, but the format is plain YAML:

```yaml
ports:
    22:
        locked: true
    3000:
        label: dev server
        favorite: true
    9000: {}
```

An entry can have a `label`, be marked `favorite`, and/or be `locked` (a locked
port can't be served until you unlock it; `:22` ships locked). An empty entry
(`{}`, like `9000`) means "keep this in the default view" without any of those —
the state left behind by serving a port without labeling or favoriting it.
tailport also records a `last_process` key per port automatically (the name it
last saw listening, used for the `was <name>` display); you don't set that by
hand.

### Status markers

A top-level `markers` key (or the `--markers` flag, which wins for that run
only) selects how each route sub-row's marker is drawn:

```yaml
markers: "" # "" / mono (default) | auto | emoji | ascii
```

- **unset (`""`, the default) — mono:** ○ localhost · ◔ local network ·
  ◑ on tailnet · ◉ served · ● public (funnel) · ◆ public (published) ·
  ◈ public (cloudflare tunnel) · ▲ stale (dangling forward) · ✕ offline.
- **`auto`** — detects a UTF-8-capable terminal (UTF-8 locale, and `TERM` isn't
  the bare Linux console or `dumb`) and switches to the moon-phase emoji ramp
  there, otherwise falls back to mono: 🌕 localhost · 🌔 local network ·
  🌓 on tailnet · 🌒 served · 🌑 public (funnel) · 🌐 public (published) ·
  ☁️ public (cloudflare tunnel) · 🌫️ stale · ✕ offline.
- **`emoji`** — always the moon-phase ramp, regardless of terminal.
- **`ascii`** — always mono, regardless of terminal (same glyphs as unset).

This setting governs the exposure markers only. Any other emoji/animation
tailport might render (e.g. its hidden Easter-egg overlay) auto-detects terminal
capability on its own, independent of `markers`.

### Theme (light/dark terminals)

tailport auto-detects your terminal's background and picks legible colors either
way. If detection guesses wrong (common over SSH/tmux/some multiplexers),
override it with a top-level `theme` key:

```yaml
theme: auto # auto (default) | light | dark
```

or the `--theme` flag, which wins over the config value. `auto` detects the
background itself; when it can't tell at all, it falls back to `dark`, so
existing dark-terminal setups see no change either way.

### Publish (Caddy edge)

*Advanced — only needed if you use the `p` publish path.*

A `caddy` block configures the optional
[publish-to-the-internet path](#publishing-to-the-public-internet-caddy-edge).
Unlike the port registry, tailport writes this block in full — with visible
defaults and explanatory comments — the first time it saves the config, so the
knobs are discoverable without reading docs:

> **Upgraded from an older tailport?** A `config.yaml` written before this
> feature landed has **no `caddy:` block yet** — that's expected, and it's why
> there's no `domain:` line to edit. It appears on the next save (any change
> that writes the file, e.g. favoriting or labeling a port), or paste the block
> below in by hand and set `domain:` there. Pressing `p` with a blank `domain`
> also captures it inline and saves it, rather than refusing.

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

- **`hostname`** (default `caddy`) — the edge's own private tailnet identity,
  used only so tailport can find its admin API at
  `http://<hostname>:<admin_port>`. Use the short MagicDNS label, **not** an
  FQDN — the edge admits only its short name, so an FQDN silently 403s. It has
  nothing to do with any published route's public hostname (e.g.
  `app.example.com`): private edge identity and public route identity are
  deliberately separate.
- **`domain`** (default `""`) — the public base domain publish hostnames are
  built from. Blank doesn't block publishing: pressing `p` captures the domain
  inline and saves it before continuing, and until it's set the background
  published-state poll doesn't run (zero cost until you set it).
- **`server_name`** (default `tailport`) — the shared Caddy HTTP server tailport
  manages. Every tailport computer publishing through the same edge must agree
  on this value; it selects the routes array, it does not identify the source
  computer.
- **`admin_port`** (default `2019`) — the Caddy admin API's port on the edge.
- **`auth_user`** / **`auth_hash`** — unset (no auth) until you opt into basic
  auth at a publish confirmation. `auth_hash` is always a bcrypt hash of the
  password you typed then, never the plaintext; every published route that opts
  into auth shares this one credential — it isn't per-hostname.
- **`silent_republish`** (default `false`) — skips the `p` key's y/n confirm
  when re-publishing a port already published earlier in the **same** session
  (its hostname and auth are remembered in memory only). A port's first publish
  this session always confirms regardless, and Funnel's confirm is unaffected.

None of this configures the edge itself — it only tells tailport where an
**already-deployed** edge lives. Standing up the edge (on Fly.io or any host you
run — Tailscale ACL and auth key, DNS) is a separate one-time operator task; see
[`docs/caddy-edge.md`](docs/caddy-edge.md).

### Tunnel (Cloudflare)

*Advanced — only needed if you use the `o` Cloudflare tunnel path.*

A `cloudflared` block configures the optional
[tunnel-to-the-internet path](#tunnelling-to-the-public-internet-cloudflare-tunnel).
Like the `caddy` block, tailport writes it in full — with visible defaults and
comments — the first time it saves the config.

> **Upgraded from an older tailport?** A `config.yaml` from before this feature
> has **no `cloudflared:` block yet** — expected. It appears on the next save,
> or paste the block below in by hand. Unlike `caddy.domain`,
> `cloudflared.domain` gates nothing: it's a pure convenience prefill.

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

- **`binary`** (default `""`) — path to the cloudflared executable. Blank means
  tailport looks it up on `$PATH`; the whole `o` feature (key, discovery,
  polling) stays dormant unless it's found there (or at this path).
- **`domain`** (default `""`) — a public base domain used only to **prefill**
  the hostname prompt when starting a **named** tunnel. Purely a convenience:
  leaving it blank blocks nothing, and a **quick** tunnel ignores it entirely.

None of this configures Cloudflare itself — for a named tunnel, logging in
(`cloudflared tunnel login`), creating the tunnel, and routing its hostname
(`cloudflared tunnel route dns`) are a separate, one-time operator task;
tailport only ever *runs* an already-provisioned named tunnel.

## Publishing to the public internet (Caddy edge)

Tailnet `serve` and Funnel aren't the only way out: tailport can publish a port
to a **custom public hostname** — `https://app.example.com`, no port in the URL,
no `*.ts.net` — through a Caddy edge node you run yourself (on Fly.io, or any
host that meets the requirements). This is a third exposure level,
architecturally independent of both `serve` and Funnel: Tailscale's role here is
private WireGuard transport from the edge to your machine, and nothing more —
Caddy owns the entire public trust plane (custom-domain DNS, `:443` ingress, TLS
termination and certificate renewal, hostname routing).

Publish and Funnel are **independent, not ranked**, and can coexist: a port can
be funnelled *and* published at once, each its own route row with its own marker
and URL. Each still requires its own strong per-service confirm before going
live, and `:22` stays hard-blocked from both.

**Setup is a separate, one-time operator task**, not something tailport does for
you: a Caddy edge deployed and reachable on your tailnet, a domain whose DNS
points at it, and a Tailscale auth key for the edge itself. See
[`docs/caddy-edge.md`](docs/caddy-edge.md) for the full runbook (written for a
Caddy/Fly first-timer) and the [`caddy.*` fields](#publish-caddy-edge)
for what tailport needs once that edge exists.

Once configured, publishing works the same shape as Funnel: select a port,
confirm the public hostname and (optionally) a shared basic-auth credential, and
confirm again against the exact `https://` URL before anything goes live.

### Publish is a toggle (`p`)

`p` behaves differently depending on the port's state:

- **Already published** — `p` unpublishes immediately. No confirm: reducing
  exposure is never gated.
- **Published earlier this session, then unpublished** — tailport remembers that
  port's hostname and auth in memory (never written to config; the edge stays
  the source of truth) for as long as the process runs. Pressing `p` again
  re-publishes with that remembered config, skipping the setup prompts — straight
  to the y/n confirm naming the exact `https://<hostname>`, unless you've set
  `silent_republish: true`, in which case it re-publishes with no confirm.
- **Never published this session** — `p` runs the full setup: hostname, optional
  basic auth, then the confirm. This always happens on a port's first publish,
  regardless of `silent_republish`.

Press **`e`** to change a published port's **auth** without unpublishing it
first: `e` runs the setup flow (prefilled with the port's current hostname) and
ends in the same y/n confirm, then updates the live route in place — the port is
never briefly unpublished in between. `e` **cannot move a still-published port to
a new hostname**: that would be a non-atomic delete-and-create that could leave
the old route dangling, so tailport refuses it with a message telling you to
unpublish (`p`) first and re-publish at the new hostname.

## Tunnelling to the public internet (Cloudflare Tunnel)

`serve`, Funnel, and Publish still aren't the only way out: tailport can tunnel
a port to the public internet through a
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/install-and-setup/installation/),
run by the `cloudflared` CLI. This is a **third** public path, sibling to Funnel
and Publish rather than layered above either — and architecturally different
from both: cloudflared is a **long-running local process** tailport supervises
directly, not a remote edge tailport pokes (Publish) or a Tailscale-managed
ingress slot (Funnel). The `cloudflared` binary *is* the connector; a tunnel is
up only while its process stays alive.

The whole feature exists only when `cloudflared` is installed: tailport detects
it once at startup, and when it's absent the `o` key is dropped from the bar
entirely — no key, no discovery, no polling, zero cost. There are two flavors,
matching Cloudflare's two account scenarios:

- **Quick tunnel** — no Cloudflare account needed. tailport runs
  `cloudflared tunnel --url http://localhost:<port>`, which hands back a random
  `https://<name>.trycloudflare.com` hostname: unauthenticated, and ephemeral —
  a new hostname every time you start one.
- **Named tunnel** — for an authenticated account. You've already run
  `cloudflared tunnel login`, created a tunnel, and routed a stable custom
  hostname to it (`cloudflared tunnel route dns`) — a separate, one-time
  operator task, like standing up the Caddy edge, that tailport never automates.
  tailport only *runs* that pre-provisioned tunnel
  (`cloudflared tunnel run --url http://localhost:<port> <name>`); it never
  mutates your Cloudflare account or DNS.

**Tunnels survive tailport exiting.** cloudflared is started detached, in its
own session, so quitting the TUI doesn't drop the tunnel — it keeps running
until you tear it down or kill it yourself. tailport never persists tunnel state
to disk; instead it reads the OS process table live on every poll, so a tunnel
started in a previous session is re-discovered next launch and stays
re-toggleable with `o`. Only **tailport-owned** tunnels — the ones carrying a
sentinel `--logfile` flag tailport always passes — are tracked this way; a
`cloudflared` process started outside tailport is left alone entirely.

Like the other public paths, Tunnel is independent and may coexist with Funnel
and Publish on the same port, every path still requires its own per-service
confirm, and `:22` stays hard-blocked. A tunnelled service shows its own
`cloudflare` route row (marker `◈` / ☁️) with the exact public URL once known.

### The tunnel toggle (`o`)

`o` behaves differently depending on the port's state:

- **Already tunnelled** — `o` tears it down immediately. No confirm.
- **Tunnelled earlier this session, then torn down** — tailport remembers that
  port's mode (and, for a named tunnel, its hostname) in memory for as long as
  the process runs, and `o` re-raises it, skipping setup — straight to the
  confirm.
- **Never tunnelled this session** — `o` runs the full setup. If you're logged
  in to Cloudflare, you pick quick or named; choosing named asks for the
  hostname you've routed and the tunnel's name. Without an account, only the
  quick path exists, so setup skips straight to its confirm.

Every path ends in a y/n confirm before anything goes live, and `:22` is
hard-blocked. The **quick** tunnel's confirm is the one deliberate exception to
the "always name the exact public URL" rule: cloudflared assigns the
`*.trycloudflare.com` hostname only after the tunnel starts, so there's no URL
to name in advance. The confirm names the local port instead; tailport flashes
`starting Cloudflare quick tunnel for :<port>…`, and the real `https://…`
address appears in the row a few seconds later, once the next poll picks it up.
A **named** tunnel's confirm has no such gap — it names the exact
`https://<hostname>` up front, the same as Publish.

## How it works

- **Port discovery:** `ss -H -t -l -n -p` on Linux, `lsof -iTCP -sTCP:LISTEN -n
  -P` on macOS, run locally — tailport never scans the network.
- **Serve status:** `tailscale serve status --json`, parsed to find which ports
  currently have an active HTTP mapping.
- **Toggling on:** `tailscale serve --bg --http=<port> <port>`.
- **Toggling off:** `tailscale serve --http=<port> off` — a surgical removal of
  just that one mapping; other active mappings are left alone.
- **Registry writes:** the config file is rewritten immediately after every
  toggle, label, favorite/unfavorite, or lock/unlock — there's no
  in-memory-only state to lose if tailport is killed rather than quit normally.

tailport has no dependencies beyond the `tailscale` CLI and the OS tools above
(and, only if you use the `o` tunnel feature, `cloudflared`) — no daemon,
nothing installed or modified system-wide other than the `serve` mappings you
toggle yourself.

## Troubleshooting

### Dangling forward (`▲` / `🌫️`, "bound to tailnet, but stale")

A row marked `▲` / `🌫️` — whose description reads *"bound to tailnet, but stale —
t to unbind"* — means the `serve` mapping is up but no local process holds
the port. Two common cases:

- **The app just isn't running** (it died, or hasn't started). Start it, or
  unbind the port — `t` on the row, or `C` to clear all stale forwards. The
  mapping deliberately outlives the app so you can restart it freely, so
  tailport won't tear it down for you.
- **The app can't start with "address already in use."** When you serve `:8025`,
  tailscaled binds your **tailnet IP** on `:8025`. If your app then tries to bind
  `0.0.0.0:8025` (all interfaces), that collides and the app fails to start — so
  the forward dangles. The mapping meant to serve the app is what's blocking it.

  The fix is to bind the app to **loopback**, which is what `serve` proxies to
  anyway:

  ```sh
  mailpit --listen 127.0.0.1:8025      # e.g. — bind 127.0.0.1, not 0.0.0.0
  ```

  This resolves the collision and keeps the app off your LAN — reachable only
  over the tailnet, through `serve`. If you genuinely need the app on
  `0.0.0.0:<port>`, unbind the port first (`t`, or `C`) — note that once
  it's on `0.0.0.0` it's already reachable on the tailnet on its own (state
  `on tailnet`), so there's nothing left to serve.

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
`lsof` code path in `internal/portscan` is compiled when cross-compiling but is
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
