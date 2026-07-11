<!-- BEGIN KATA (managed by `kata init --with-agents`) -->
## kata issue tracker

This project uses [kata](https://github.com/kenn-io/kata) as its shared issue
ledger. Run `kata quickstart` at the start of each session for the full agent
contract. The short version:

- Search before creating: `kata search "<keywords>" --agent`.
- Prefer updating existing issues over duplicates (`kata comment`, `kata label add`, `kata edit`).
- Default to `--agent` for ordinary reads and mutations; use `--json` only when a script needs structured data.
- Close only verified work: `kata close <ref> --done --message "<scope + verification>" --commit <sha>`.
- If work is incomplete, label `needs-review` and comment what remains rather than closing.
- Never `kata delete` or `kata purge` without explicit user authorization.
<!-- END KATA -->

## Project rules

### Design constraints (do not relax without asking)

- Tailnet-first. `tailscale serve` (tailnet-only exposure) is the default
  path. `tailscale funnel` (public internet exposure) IS supported, but only
  as a deliberate, per-service opt-in via the `p` key behind a strong y/n
  confirm that names the port and shows the resulting public URL. `:22` (SSH)
  is hard-blocked from funnel. Never funnel implicitly, in bulk, or without
  that confirm. (Implemented under kata yt69: the `p` key, `entryConfirmFunnel`
  gate, and `tsserve.FunnelOn/FunnelOff/FunnelStatus`.)
- Serve (tailnet) is plain HTTP only (`--http=PORT`). No HTTPS/TLS serve
  mode — deliberate, see project history: Tailscale's WireGuard tunnel
  already encrypts peer-to-peer traffic, so app-layer TLS added no real
  confidentiality here, and it would have pulled in cert/HTTPS complexity
  for no benefit. Funnel is necessarily different: its public ingress is
  always HTTPS/TLS (Tailscale terminates TLS with the node's `ts.net` cert;
  there is no plain-HTTP funnel). The local proxy target stays plain
  `http://127.0.0.1:PORT` either way.
- 1:1 port mapping for serve (tailnet) — the exposed tailnet port always
  equals the local port; no remapping. Funnel is exempt because Tailscale
  restricts funnel ingress to ports `443`, `8443`, and `10000` only: the
  local target port is unrestricted, but the public port is one of those
  three (auto-assigned 443 → 8443 → 10000, max three concurrent funnels
  per node). Serve mappings stay strictly 1:1.
- Fleet targets: `linux/amd64` (host-a, host-b) and `darwin/arm64` (mac-a,
  mac-b, mac-c). Keep `.github/workflows/build.yml` and `install.sh` in
  sync with this list if it changes.
- Zero non-Go runtime dependencies in the shipped binary. It shells out
  to `tailscale`, and to `ss` (Linux) / `lsof` (macOS) for port discovery
  — nothing else *required*. Don't add a dependency on `yq`, `gum`, `fzf`,
  etc.; config parsing uses `gopkg.in/yaml.v3` natively for this reason.
  Carve-out (vnq7): an OPTIONAL, best-effort clipboard helper
  (`pbcopy` / `wl-copy` / `xclip` / `xsel`) may be shelled out to for the
  `c` copy-URL action, but it is never required to build or run — the
  primary clipboard path is OSC 52 (pure Go, no external binary), and a
  missing helper is silently skipped.

### Verification bar

- A kata issue does not close on "it compiles." Run `go build ./...`,
  `go vet ./...`, and `go test ./...` at minimum. Where the change is
  user-visible (TUI behavior, a CLI flag, an actual `tailscale serve`
  interaction), exercise it for real — e.g. a detached `tmux` session
  driving the compiled binary with `send-keys`/`capture-pane`, or a
  throwaway `go run` harness against live `tailscaled` — and cite what
  you actually observed in the close message, not just that it built.
- If something can't be verified in the current environment (e.g. the
  macOS `lsof` path with no Mac available, or a GitHub Actions run with
  no pushed remote), say so explicitly. Either leave the issue open with
  `needs-review` and a comment describing exactly what's blocked, or close
  it with an honest caveat in the message — never claim untested code
  paths as verified.

### Workflow

- Use kata for all real feature/bug work in this repo. Search before
  creating, claim before starting, close only with evidence.
- Parallel feature work happens in git worktrees, one subagent per
  feature branch, rebased (not merge-commit) into `main` once its kata
  issue is closed with verification.
- No force-push, no `git reset --hard`, no skipping hooks, without
  explicit user authorization for that specific action.
