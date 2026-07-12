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
opt-in: the `p` key funnels the selected port via `tailscale funnel`, behind
a strong y/n confirmation. Funnel is HTTPS-only and uses one of Tailscale's
three public ingress ports (443, 8443, 10000), so a funnelled port is
reachable by anyone on the internet — not just your tailnet. Public exposure
is never automatic; it happens only when you press `p` and confirm, `:22`
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
  sudo tailscale set --operator=$USER
  ```
- Linux (uses `ss` for port discovery) or macOS (uses `lsof`). Other
  platforms aren't supported.
- Prebuilt release binaries are published for `linux/amd64`,
  `linux/arm64`, and `darwin/arm64` (see Install below). Other
  OS/architecture combinations require building from source with `go
  install`.

## Install

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
curl -fsSL https://raw.githubusercontent.com/gruen/tailport/main/install.sh | bash
```

The script detects your OS and architecture, downloads the matching binary
from the latest release, and installs it to `~/.local/bin/tailport` (override
the destination directory with `TAILPORT_INSTALL_DIR`). Release binaries are
built by `.github/workflows/build.yml` on tagged pushes (`v*`); if that
workflow's build matrix doesn't cover your platform, use `go install`
instead.

## Usage

Run `tailport`. It scans locally listening TCP ports and shows which ones
are currently exposed on your tailnet.

| Key | Action |
| --- | --- |
| `space` | Toggle `tailscale serve` (tailnet-only) on/off for the selected port |
| `p` | Funnel the selected port to the **public internet** via `tailscale funnel`, behind a strong y/n confirm (`:22` refused). Press again to drop it back to tailnet-served |
| `c` | Copy the selected port's tailnet URL to the clipboard (via OSC 52, so it works over SSH) |
| `C` | Tear down stale forwards — ports still served with nothing listening locally. Offered only when some exist |
| `x` | Lock / unlock the selected port. A locked port can't be served until unlocked; `:22` is locked by default and unlocking it requires typing `ssh` |
| `n` | Add a port by number to Favorites (even one nothing is listening on yet). It does **not** serve — press `space` there to expose it once its service is up |
| `l` | Label the selected port with custom text (prefilled with its current label if set, else the process name) |
| `f` | Favorite the selected port, pinning it to the default view |
| `u` | Unfavorite the selected port |
| `a` | Toggle between the default view and showing every listening port |
| `/` | Filter by port number, process, or label (fuzzy) |
| `r` | Refresh the port list and serve status |
| `?` | Toggle the full help overlay |
| `q` / `ctrl+c` | Quit |

Each row's leading marker encodes the port's exposure state — idle,
tailnet-served, public (funnel), or served-but-nothing-listening; see
[Status markers](#status-markers) below for the exact glyphs. A
tailnet-served port shows the `http://<hostname>:<port>` URL it's reachable
at from other tailnet devices; a funnelled port shows its public HTTPS URL
instead. A favorited port additionally shows a star (★). The name shown next
to a port is its custom label if you've set one, otherwise its resolved
process name (or `was <name>` for a favorite whose process has since exited)
— or `?` if that can't be determined, which happens when the port belongs to
a process owned by a different user (most commonly `root`) than the one
running tailport.

### Default view and the port registry

tailport doesn't show every listening port by default — that gets noisy
fast (sshd, mDNS, Docker, browsers holding sockets open, etc.). Instead it
shows the union of:

- ports currently exposed via `tailscale serve`, and
- ports in the **registry**: anything you've ever toggled on, labeled, or
  favorited.

A port earns a place in the registry the moment you interact with it —
serving it (`space`), adding it by number (`n`), labeling it (`l`),
favoriting it (`f`), or locking it (`x`) all add it. Once a port is in the
registry it keeps showing up, marked inactive, even after you toggle it off —
and that persists across restarts, not just for the current session. `u` on a
port that has no label and isn't locked reverses this: it's dropped from the
registry and disappears from the default view (unless it's currently active).

Press `a` to bypass the registry entirely and see every port currently
listening on the machine, whether known to tailport or not — useful for
finding something new to expose, label, or favorite.

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

A top-level `markers` key selects how a port's exposure state is drawn:

```yaml
markers: auto # auto (default) | emoji | ascii
```

- `auto` — egg-lifecycle emoji on a UTF-8-capable terminal (locale is
  UTF-8 and `TERM` isn't the bare Linux console or `dumb`), otherwise ASCII.
- `emoji` — always 🥚 idle · 🐣 tailnet-served · 🐦 public (funnel) ·
  🪹 served but nothing listening.
- `ascii` — always ○ idle · ◉ tailnet-served · ● public (funnel) ·
  ▲ served but nothing listening.

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

## Troubleshooting

### Dangling forward (`▲` / `🪹`, "bound to tailscale")

A row marked `▲` / `🪹` — whose description reads *"bound to tailscale, press
space to release/unbind"* — means the `serve` mapping is up but no local
process holds the port. Two common cases:

- **The app just isn't running** (it died, or hasn't started). Start it, or
  un-expose the port — `space` on the row, or `C` to clear all stale forwards.
  The mapping deliberately outlives the app so you can restart it freely, so
  tailport won't tear it down for you.
- **The app can't start with "address already in use."** When you serve
  `:8025`, tailscaled binds your **tailnet IP** on `:8025`. If your app then
  tries to bind `0.0.0.0:8025` (all interfaces), that collides and the app
  fails to start — so the forward dangles. The mapping meant to expose the app
  is what's blocking it.

  The fix is to bind the app to **loopback**, which is what `serve` proxies to
  anyway:

  ```sh
  mailpit --listen 127.0.0.1:8025      # e.g. — bind 127.0.0.1, not 0.0.0.0
  ```

  This both resolves the collision and keeps the app off your LAN — it's
  reachable only over the tailnet, through `serve`. If you genuinely need the
  app on `0.0.0.0:<port>`, un-expose the port first (`space`, or `C`).

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
