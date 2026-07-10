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

tailport only ever exposes ports to your **tailnet** — the private network
of devices you've authenticated into Tailscale. It does this by shelling
out to `tailscale serve`.

It **never** invokes `tailscale funnel`, which would expose a port to the
public internet. There is no flag, mode, or code path in tailport that does
this. If you want public exposure, use the `tailscale` CLI directly and
funnel is not something tailport will do for you.

Two other constraints, both deliberate:

- **Plain HTTP only** (`tailscale serve --http=<port>`), never HTTPS/TLS
  serve mode. Tailscale's WireGuard tunnel already encrypts traffic between
  tailnet peers, so app-layer TLS on top wouldn't add real confidentiality
  here — it would just add certificate handling for no benefit.
- **1:1 port mapping only.** The port exposed on your tailnet is always the
  same number as the local port. tailport does not support remapping a
  local port to a different public-facing port.

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
- Prebuilt release binaries are currently only published for
  `linux/amd64` and `darwin/arm64` (see Install below). Other
  OS/architecture combinations require building from source with `go
  install`.

## Install

**With Go installed**, for any supported OS/arch:

```sh
go install github.com/gruen/tailport/cmd/tailport@latest
```

**Without Go**, on `linux/amd64` or `darwin/arm64`, fetch a prebuilt binary
from this repo's [GitHub Releases](https://github.com/gruen/tailport/releases)
using the bundled install script. Either run it after cloning:

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
| `enter` / `space` | Toggle `tailscale serve` on/off for the selected port |
| `n` | Open a text-input to type a port number (even one nothing is listening on yet) and toggle it on |
| `a` | Toggle between the filtered view and showing every listening port |
| `r` | Refresh the port list and serve status |
| `q` / `ctrl+c` | Quit |

An exposed port shows a filled marker (●) and the `http://<hostname>:<port>`
URL it's reachable at from other tailnet devices; an unexposed one shows a
hollow marker (○).

### Filtered view

By default tailport hides a short list of ports that are almost never what
you want to expose (sshd, cups, avahi/mDNS, and Tailscale's own WireGuard
listener). Press `a` to see every listening port, including those. The
exclude list only affects the default view — it doesn't stop you from
exposing an excluded port via `n`, and any port already exposed is always
shown regardless of the filter.

## Configuration

On first run, tailport writes a default config to:

```
$XDG_CONFIG_HOME/tailport/config.yaml
```

or, if `XDG_CONFIG_HOME` isn't set, `~/.config/tailport/config.yaml`. It
won't overwrite an existing file. The only setting today is the filtered
view's exclude list:

```yaml
exclude_ports:
    - 22
    - 631
    - 5353
    - 41641
```

That's `22` (sshd), `631` (cups), `5353` (mdns/avahi), and `41641`
(Tailscale's own WireGuard listener). Edit this list to hide (or stop
hiding) other ports from the default view.

## How it works

- Port discovery: `ss -H -t -l -n -p` on Linux, `lsof -iTCP -sTCP:LISTEN -n
  -P` on macOS, run locally — tailport never scans the network.
- Serve status: `tailscale serve status --json`, parsed to find which ports
  currently have an active HTTP mapping.
- Toggling on: `tailscale serve --bg --http=<port> <port>`.
- Toggling off: `tailscale serve --http=<port> off` (a surgical removal of
  just that one mapping; other active mappings are left alone).

tailport has no dependencies beyond the `tailscale` CLI and the OS tools
above — no daemon, no config beyond the YAML file, and nothing is installed
or modified system-wide other than the `serve` mappings you toggle
yourself.

## License

[MIT](./LICENSE)
